package containuum

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

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

// monitor consolidates all container monitoring logic.
type monitor struct {
	ctx      context.Context
	client   DockerClient
	callback Callback
	filter   Filter

	// Timing config
	debounce        time.Duration
	maxDebounceTime time.Duration
	maxIdleTime     time.Duration

	// Reconnect config (nil = disabled)
	reconnect *reconnectConfig

	// State
	previousHash *uint64
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

	debounceTimer := time.NewTimer(m.debounce)
	debounceTimer.Stop()
	defer debounceTimer.Stop()

	maxDebounceTimer := time.NewTimer(m.maxDebounceTime)
	maxDebounceTimer.Stop()
	defer maxDebounceTimer.Stop()

	idleTicker := time.NewTicker(m.maxIdleTime)
	defer idleTicker.Stop()

	waiting := false

	for {
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

			if waiting {
				debounceTimer.Reset(m.debounce)
			} else {
				debounceTimer.Reset(m.debounce)
				maxDebounceTimer.Reset(m.maxDebounceTime)
				waiting = true
			}

		case <-debounceTimer.C:
			if err := m.gather(); err != nil {
				return err
			}
			maxDebounceTimer.Stop()
			idleTicker.Reset(m.maxIdleTime)
			waiting = false

		case <-maxDebounceTimer.C:
			Log("Maximum debounce time exceeded, refreshing", "maxDebounceTime", m.maxDebounceTime, "debounce", m.debounce)
			if err := m.gather(); err != nil {
				return err
			}
			debounceTimer.Stop()
			idleTicker.Reset(m.maxIdleTime)
			waiting = false

		case <-idleTicker.C:
			Log("Maximum idle time exceeded, refreshing", "maxIdleTime", m.maxIdleTime)
			if err := m.gather(); err != nil {
				return err
			}
			if waiting {
				debounceTimer.Stop()
				maxDebounceTimer.Stop()
				waiting = false
			}
		}
	}
}

// gather retrieves containers, deduplicates, and invokes the callback.
func (m *monitor) gather() error {
	containers, err := m.gatherContainers()
	if err != nil {
		Log("Failed to refresh containers", "error", err)
		return fmt.Errorf("failed to refresh containers: %w", err)
	}

	// Deduplicate
	currentHash := computeHash(containers)
	if m.previousHash != nil && currentHash == *m.previousHash {
		Log("Container state unchanged, not invoking callback")
		return nil
	}

	Log("Container state changed, invoking callback", "count", len(containers))
	m.previousHash = &currentHash
	m.callback(containers)
	return nil
}

// gatherContainers retrieves all containers, applies filters, and returns the matching set.
func (m *monitor) gatherContainers() ([]Container, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()

	list, err := m.client.ContainerList(ctx, client.ContainerListOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	var containers []Container
	for _, summary := range list.Items {
		inspect, err := m.client.ContainerInspect(ctx, summary.ID, client.ContainerInspectOptions{})
		if err != nil {
			Log("Failed to inspect container", "id", summary.ID, "error", err)
			continue
		}

		c := convertContainer(inspect.Container)
		if m.filter == nil || m.filter(c) {
			containers = append(containers, c)
		}
	}

	return containers, nil
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
