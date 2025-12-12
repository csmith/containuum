package containuum

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
)

// eventFilters are the Docker events we subscribe to.
var eventFilters = filters.NewArgs(
	filters.Arg("type", "container"),
	filters.Arg("type", "network"),
	filters.Arg("event", "create"),
	filters.Arg("event", "start"),
	filters.Arg("event", "stop"),
	filters.Arg("event", "die"),
	filters.Arg("event", "kill"),
	filters.Arg("event", "pause"),
	filters.Arg("event", "unpause"),
	filters.Arg("event", "rename"),
	filters.Arg("event", "update"),
	filters.Arg("event", "destroy"),
	filters.Arg("event", "connect"),
	filters.Arg("event", "disconnect"),
)

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
	eventCh, errCh := m.client.Events(m.ctx, events.ListOptions{
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

		case err := <-errCh:
			if err != nil {
				return fmt.Errorf("failed to stream events: %w", err)
			}
			return nil

		case event := <-eventCh:
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

	summaries, err := m.client.ContainerList(ctx, container.ListOptions{
		All: true,
	})
	if err != nil {
		return nil, err
	}

	var containers []Container
	for _, summary := range summaries {
		inspect, err := m.client.ContainerInspect(ctx, summary.ID)
		if err != nil {
			Log("Failed to inspect container", "id", summary.ID, "error", err)
			continue
		}

		c := convertContainer(inspect)
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
		State:  inspect.State.Status,
		Labels: inspect.Config.Labels,
	}

	if inspect.NetworkSettings != nil {
		for name, network := range inspect.NetworkSettings.Networks {
			c.Networks = append(c.Networks, Network{
				Name:       name,
				ID:         network.NetworkID,
				IPAddress:  network.IPAddress,
				IP6Address: network.GlobalIPv6Address,
				Gateway:    network.Gateway,
				Aliases:    network.Aliases,
			})
		}
	}

	if inspect.NetworkSettings != nil {
		for portProto, bindings := range inspect.NetworkSettings.Ports {
			parts := strings.Split(string(portProto), "/")
			if len(parts) != 2 {
				continue
			}

			containerPortNum, err := strconv.ParseUint(parts[0], 10, 16)
			if err != nil {
				continue
			}
			containerPort := uint16(containerPortNum)

			for _, binding := range bindings {
				hostPortNum, err := strconv.ParseUint(binding.HostPort, 10, 16)
				if err != nil {
					continue
				}

				c.Ports = append(c.Ports, Port{
					HostIP:        binding.HostIP,
					HostPort:      uint16(hostPortNum),
					ContainerPort: containerPort,
					Protocol:      parts[1],
				})
			}
		}
	}

	return c
}

// computeHash generates a hash of the container list.
func computeHash(containers []Container) uint64 {
	var hash uint64
	for i := range containers {
		hash ^= containers[i].hash()
	}
	return hash
}
