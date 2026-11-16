package containuum

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// eventFilters are the Docker events we subscribe to.
var eventFilters = client.Filters{}.
	Add("type", "container").
	Add("type", "network").
	Add("event", "create").
	Add("event", "start").
	Add("event", "stop").
	Add("event", "die").
	Add("event", "kill").
	Add("event", "pause").
	Add("event", "unpause").
	Add("event", "rename").
	Add("event", "update").
	Add("event", "destroy").
	Add("event", "connect").
	Add("event", "disconnect")

// reconnectConfig holds parameters for automatic reconnection.
type reconnectConfig struct {
	MinDelay   time.Duration
	MaxDelay   time.Duration
	MaxRetries int
}

const (
	// listTimeout bounds the call to list containers.
	listTimeout = 30 * time.Second
	// maxConcurrentInspects is how many inspects may run at once, not
	// counting stalled ones.
	maxConcurrentInspects = 16
	// inspectStallTime is how long a refresh waits for an inspect before
	// treating the container as stalled.
	inspectStallTime = 5 * time.Second
	// initialInspectStallTime replaces inspectStallTime until the first
	// emission, when there is no previous state to fall back on.
	initialInspectStallTime = 30 * time.Second
	// inspectTimeout is how long a stalled inspect is left open for.
	inspectTimeout = 5 * time.Minute
)

// monitor consolidates all container monitoring logic.
type monitor struct {
	ctx          context.Context
	client       DockerClient
	callback     Callback
	errorHandler func(error)
	filter       Filter

	// Timing config
	debounce        time.Duration
	maxDebounceTime time.Duration
	maxIdleTime     time.Duration

	// Reconnect config (nil = disabled)
	reconnect *reconnectConfig

	// State
	previousHash *uint64
	// order is the container IDs from the last list, in list order.
	order      []string
	containers map[string]*containerState
	// refreshWanted is set when an inspect that was outstanding during a
	// refresh succeeds, as its result may predate the refresh.
	refreshWanted bool

	// Inspect requests outlive refreshes and reconnects. They deliver to
	// results, hold a slot while running and not stalled, and are tracked by
	// wg so Run can wait for them.
	results chan inspectResult
	slots   chan struct{}
	wg      sync.WaitGroup
}

// containerState is what the monitor knows about a listed container.
type containerState struct {
	summary container.Summary
	// last is the result of the last successful inspect, or nil if there
	// hasn't been one.
	last *Container
	// failing is set once an inspect fails or stalls, and cleared when one
	// succeeds.
	failing bool
	// inflight is the outstanding inspect request, if any.
	inflight *inspectRequest
}

type inspectRequest struct {
	id     string
	stall  time.Duration
	cancel context.CancelFunc
	// refreshed is set if a refresh happened while the request was outstanding.
	refreshed bool
}

// inspectResult is a request's outcome, or with stalled set, notice that it
// is taking longer than its stall time. A stall notice always comes first.
type inspectResult struct {
	req     *inspectRequest
	stalled bool
	inspect container.InspectResponse
	err     error
}

func newMonitor(ctx context.Context) *monitor {
	return &monitor{
		ctx:        ctx,
		containers: make(map[string]*containerState),
		results:    make(chan inspectResult),
		slots:      make(chan struct{}, maxConcurrentInspects),
	}
}

// run starts monitoring and blocks until context is cancelled or an error occurs.
func (m *monitor) run() error {
	if m.reconnect != nil {
		return m.runWithRetry()
	}
	return m.runOnce()
}

// runWithRetry wraps runOnce with exponential backoff retry logic.
func (m *monitor) runWithRetry() error {
	attempt := 0
	delay := m.reconnect.MinDelay

	for {
		startTime := time.Now()
		err := m.runOnce()

		if m.ctx.Err() != nil {
			return m.ctx.Err()
		}

		if time.Since(startTime) >= time.Minute {
			attempt = 0
			delay = m.reconnect.MinDelay
		}

		attempt++
		if m.reconnect.MaxRetries > 0 && attempt > m.reconnect.MaxRetries {
			Log("Max reconnect retries exceeded", "attempts", attempt)
			return err
		}

		Log("Event stream disconnected, will reconnect", "attempt", attempt, "delay", delay, "error", err)
		if err == nil {
			err = errors.New("docker event stream closed")
		}
		if m.errorHandler != nil {
			m.errorHandler(fmt.Errorf("reconnecting after error: %w", err))
		}

		select {
		case <-m.ctx.Done():
			return m.ctx.Err()
		case <-time.After(delay):
		}

		Log("Reconnecting to docker event stream")

		delay *= 2
		if delay > m.reconnect.MaxDelay {
			delay = m.reconnect.MaxDelay
		}
	}
}

// runOnce is the main event loop.
func (m *monitor) runOnce() error {
	events := m.client.Events(m.ctx, client.EventsListOptions{
		Filters: eventFilters,
	})

	Log("Subscribed to docker events")

	// Emit initial state immediately
	if err := m.gather(); err != nil {
		return err
	}

	d := newDebouncer(m.debounce, m.maxDebounceTime)
	defer d.stop()

	idleTicker := time.NewTicker(m.maxIdleTime)
	defer idleTicker.Stop()

	for {
		if m.refreshWanted {
			m.refreshWanted = false
			Log("Inspect outstanding during a refresh succeeded, scheduling refresh")
			d.schedule(true)
		}

		select {
		case <-m.ctx.Done():
			return m.ctx.Err()

		case err := <-events.Err:
			if err != nil {
				return fmt.Errorf("failed to stream events: %w", err)
			}
			return nil

		case event := <-events.Messages:
			Log("Received event from docker", "type", event.Type, "actor", event.Actor.ID, "action", event.Action)
			idleTicker.Reset(m.maxIdleTime)
			d.schedule(true)

		case res := <-m.results:
			if m.handle(res) {
				d.schedule(false)
			}

		case <-d.timer.C:
			if err := m.flush(d.take(), idleTicker); err != nil {
				return err
			}

		case <-d.maxTimer.C:
			Log("Maximum debounce time exceeded", "maxDebounceTime", m.maxDebounceTime, "debounce", m.debounce)
			if err := m.flush(d.take(), idleTicker); err != nil {
				return err
			}

		case <-idleTicker.C:
			Log("Maximum idle time exceeded, refreshing", "maxIdleTime", m.maxIdleTime)
			d.take()
			if err := m.gather(); err != nil {
				return err
			}
		}
	}
}

// flush handles the debounce timers firing: it refreshes containers if
// refresh is set, otherwise it emits the stored state.
func (m *monitor) flush(refresh bool, idleTicker *time.Ticker) error {
	if !refresh {
		m.emit()
		return nil
	}
	idleTicker.Reset(m.maxIdleTime)
	return m.gather()
}

// debouncer coalesces requests to refresh or emit containers. Each request
// restarts timer, and maxTimer bounds how long requests can keep doing so.
type debouncer struct {
	timer      *time.Timer
	maxTimer   *time.Timer
	debounce   time.Duration
	max        time.Duration
	waiting    bool
	refreshDue bool
}

func newDebouncer(debounce, maxDebounce time.Duration) *debouncer {
	d := &debouncer{
		timer:    time.NewTimer(debounce),
		maxTimer: time.NewTimer(maxDebounce),
		debounce: debounce,
		max:      maxDebounce,
	}
	d.stop()
	return d
}

// schedule requests an emit, or a full refresh if refresh is set.
func (d *debouncer) schedule(refresh bool) {
	d.refreshDue = d.refreshDue || refresh
	d.timer.Reset(d.debounce)
	if !d.waiting {
		d.maxTimer.Reset(d.max)
		d.waiting = true
	}
}

// take stops the timers and clears pending requests, returning whether a
// refresh was due.
func (d *debouncer) take() bool {
	refresh := d.refreshDue
	d.stop()
	d.waiting = false
	d.refreshDue = false
	return refresh
}

func (d *debouncer) stop() {
	d.timer.Stop()
	d.maxTimer.Stop()
}

// gather refreshes containers, and invokes the callback if they've changed.
func (m *monitor) gather() error {
	if err := m.refresh(); err != nil {
		Log("Failed to refresh containers", "error", err)
		return fmt.Errorf("failed to refresh containers: %w", err)
	}
	m.emit()
	return nil
}

// emit invokes the callback with the stored containers, in list order, if the
// filtered set has changed.
func (m *monitor) emit() {
	var containers []Container
	for _, id := range m.order {
		state, ok := m.containers[id]
		if !ok || state.last == nil {
			continue
		}
		c := state.last.clone()
		if m.filter == nil || m.filter(c) {
			containers = append(containers, c)
		}
	}

	currentHash := computeHash(containers)
	if m.previousHash != nil && currentHash == *m.previousHash {
		Log("Container state unchanged, not invoking callback")
		return
	}

	Log("Container state changed, invoking callback", "count", len(containers))
	m.previousHash = &currentHash
	m.callback(containers)
}

// refresh lists containers and inspects them. It waits for the inspects of
// containers that were last inspected successfully, until each returns or
// stalls. Containers that are failing are inspected again without waiting,
// and containers with an inspect still outstanding are left alone. Only a
// failed list is an error.
func (m *monitor) refresh() error {
	listCtx, cancel := context.WithTimeout(m.ctx, listTimeout)
	list, err := m.client.ContainerList(
		listCtx,
		client.ContainerListOptions{All: true},
	)
	cancel()
	if err != nil {
		return err
	}

	listed := make(map[string]bool, len(list.Items))
	m.order = m.order[:0]
	for _, summary := range list.Items {
		listed[summary.ID] = true
		m.order = append(m.order, summary.ID)
	}
	for id, state := range m.containers {
		if !listed[id] {
			if state.inflight != nil {
				state.inflight.cancel()
			}
			delete(m.containers, id)
		}
	}

	stall := inspectStallTime
	if m.previousHash == nil {
		stall = initialInspectStallTime
	}

	waiting := make(map[*inspectRequest]bool)
	for _, summary := range list.Items {
		state, ok := m.containers[summary.ID]
		if !ok {
			state = &containerState{}
			m.containers[summary.ID] = state
		}
		state.summary = summary

		if state.inflight != nil {
			state.inflight.refreshed = true
			continue
		}

		req := m.startInspect(summary.ID, stall)
		state.inflight = req
		if !state.failing {
			waiting[req] = true
		}
	}

	// A request queued behind others for a slot may not have started its own
	// stall timer yet, so bound the whole wait as well. Anything still
	// outstanding at the deadline is picked up when it returns.
	deadline := time.NewTimer(stall)
	defer deadline.Stop()

	for len(waiting) > 0 {
		select {
		case res := <-m.results:
			m.handle(res)
			delete(waiting, res.req)
		case <-deadline.C:
			Log("Inspects still outstanding at the refresh deadline", "count", len(waiting))
			return nil
		case <-m.ctx.Done():
			return m.ctx.Err()
		}
	}
	return nil
}

// startInspect starts an inspect request, which delivers its result to
// m.results.
func (m *monitor) startInspect(id string, stall time.Duration) *inspectRequest {
	ctx, cancel := context.WithTimeout(m.ctx, inspectTimeout)
	req := &inspectRequest{id: id, stall: stall, cancel: cancel}

	m.wg.Go(
		func() {
			defer cancel()

			select {
			case m.slots <- struct{}{}:
			case <-ctx.Done():
				m.deliver(inspectResult{req: req, err: ctx.Err()})
				return
			}

			done := make(chan inspectResult, 1)
			m.wg.Go(
				func() {
					inspect, err := m.client.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
					done <- inspectResult{req: req, inspect: inspect.Container, err: err}
				},
			)

			stallTimer := time.NewTimer(stall)
			defer stallTimer.Stop()

			select {
			case res := <-done:
				<-m.slots
				m.deliver(res)
			case <-stallTimer.C:
				<-m.slots
				m.deliver(inspectResult{req: req, stalled: true})
				m.deliver(<-done)
			}
		},
	)

	return req
}

// deliver passes an inspect result to the monitor, giving up if it stops.
func (m *monitor) deliver(res inspectResult) {
	select {
	case m.results <- res:
	case <-m.ctx.Done():
	}
}

// handle records an inspect result, ignoring it if the request has been
// superseded or cancelled. It returns true if the stored containers changed.
func (m *monitor) handle(res inspectResult) bool {
	state, ok := m.containers[res.req.id]
	if !ok || state.inflight != res.req {
		return false
	}

	if res.stalled {
		Log("Inspect stalled", "id", res.req.id, "after", res.req.stall)
		m.markFailing(
			state,
			fmt.Errorf(
				"inspect of container %s hasn't returned after %s",
				containerName(state.summary),
				res.req.stall,
			),
		)
		return false
	}

	state.inflight = nil
	switch {
	case res.err == nil:
		c := convertContainer(res.inspect)
		state.last = &c
		state.failing = false
		if res.req.refreshed {
			m.refreshWanted = true
		}
		return true

	case cerrdefs.IsNotFound(res.err):
		Log("Container removed before it could be inspected", "id", res.req.id)
		delete(m.containers, res.req.id)
		return true

	case m.ctx.Err() != nil:
		return false

	default:
		Log("Failed to inspect container", "id", res.req.id, "error", res.err)
		m.markFailing(
			state,
			fmt.Errorf(
				"failed to inspect container %s: %w",
				containerName(state.summary),
				res.err,
			),
		)
		return false
	}
}

// markFailing records that a container is failing, reporting the error if it
// wasn't already.
func (m *monitor) markFailing(state *containerState, err error) {
	if state.failing {
		return
	}
	state.failing = true
	if m.errorHandler != nil {
		m.errorHandler(err)
	}
}

// containerName describes a listed container by name and ID, or just ID if it
// has no name.
func containerName(summary container.Summary) string {
	if len(summary.Names) == 0 {
		return summary.ID
	}
	return fmt.Sprintf("%s (%s)", strings.TrimPrefix(summary.Names[0], "/"), summary.ID)
}

// convertContainer converts a Docker API container to our model.
func convertContainer(inspect container.InspectResponse) Container {
	c := Container{
		ID:     inspect.ID,
		Name:   strings.TrimPrefix(inspect.Name, "/"),
		Image:  inspect.Config.Image,
		State:  string(inspect.State.Status),
		Labels: inspect.Config.Labels,
	}

	if inspect.NetworkSettings != nil {
		for name, network := range inspect.NetworkSettings.Networks {
			c.Networks = append(c.Networks, Network{
				Name:       name,
				ID:         network.NetworkID,
				IPAddress:  addrString(network.IPAddress),
				IP6Address: addrString(network.GlobalIPv6Address),
				Gateway:    addrString(network.Gateway),
				Aliases:    network.Aliases,
			})
		}
	}

	if inspect.NetworkSettings != nil {
		for port, bindings := range inspect.NetworkSettings.Ports {
			if !port.IsValid() {
				continue
			}

			containerPort := port.Num()
			protocol := string(port.Proto())

			// Exposed but unbound ports have no bindings; emit with zero host port.
			if len(bindings) == 0 {
				c.Ports = append(c.Ports, Port{
					ContainerPort: containerPort,
					Protocol:      protocol,
				})
				continue
			}

			for _, binding := range bindings {
				hostPortNum, err := strconv.ParseUint(binding.HostPort, 10, 16)
				if err != nil {
					continue
				}

				c.Ports = append(c.Ports, Port{
					HostIP:        addrString(binding.HostIP),
					HostPort:      uint16(hostPortNum),
					ContainerPort: containerPort,
					Protocol:      protocol,
				})
			}
		}
	}

	return c
}

// addrString formats an address, returning an empty string for an unset one
// (which [netip.Addr.String] would format as "invalid IP").
func addrString(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}

// computeHash generates a hash of the container list.
func computeHash(containers []Container) uint64 {
	var hash uint64
	for i := range containers {
		hash ^= containers[i].hash()
	}
	return hash
}
