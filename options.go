package containuum

import (
	"context"
	"log/slog"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
)

// Log is the logging function used by the library.
// It takes a message and optional key-value pairs for structured logging.
// Defaults to slog.Debug but can be overridden.
var Log func(msg string, keysAndValues ...any) = slog.Debug

// DockerClient is the interface required from the Docker SDK.
// This is a subset of client.APIClient for easier testing and custom client injection.
type DockerClient interface {
	// Events returns a channel of Docker events and a channel of errors.
	Events(ctx context.Context, options events.ListOptions) (<-chan events.Message, <-chan error)

	// ContainerList returns a list of containers.
	ContainerList(ctx context.Context, options container.ListOptions) ([]container.Summary, error)

	// ContainerInspect returns detailed information about a container.
	ContainerInspect(ctx context.Context, containerID string) (container.InspectResponse, error)
}

// Option configures the monitor.
type Option func(*config)

// Callback is invoked when the set of matching containers changes.
type Callback func(containers []Container)

// config holds the configuration for monitoring.
type config struct {
	client              DockerClient
	filter              Filter
	debounce            time.Duration
	maxDebounceTime     time.Duration
	maxIdleTime         time.Duration
	enableAutoReconnect bool
	minReconnectDelay   time.Duration
	maxReconnectDelay   time.Duration
	maxReconnectRetries int
}

// defaultConfig returns a config with sensible defaults.
func defaultConfig() *config {
	return &config{
		debounce:        100 * time.Millisecond,
		maxDebounceTime: 5 * time.Second,
		maxIdleTime:     30 * time.Second,
	}
}

// WithDockerClient sets a custom Docker client.
// If not provided, a client will be created using default settings.
func WithDockerClient(client DockerClient) Option {
	return func(c *config) {
		c.client = client
	}
}

// WithFilter sets the filter for selecting containers.
// Use All() or Any() to combine multiple filters.
func WithFilter(filter Filter) Option {
	return func(c *config) {
		c.filter = filter
	}
}

// WithDebounce sets the debounce duration for coalescing rapid events.
// Default is 100ms.
func WithDebounce(d time.Duration) Option {
	return func(c *config) {
		c.debounce = d
	}
}

// WithMaxDebounceTime sets the maximum time to wait when debouncing.
// This prevents indefinite delays when events keep arriving.
// Default is 5 seconds.
func WithMaxDebounceTime(d time.Duration) Option {
	return func(c *config) {
		c.maxDebounceTime = d
	}
}

// WithMaxIdleTime sets the maximum time to wait before polling for changes.
// Default is 30 seconds.
func WithMaxIdleTime(d time.Duration) Option {
	return func(c *config) {
		c.maxIdleTime = d
	}
}

// WithAutoReconnect enables automatic reconnection on event stream errors.
// Uses exponential backoff starting at minDelay, doubling up to maxDelay.
// maxRetries of 0 means retry forever, otherwise stop after that many attempts.
// On successful reconnection, containers will be refreshed.
func WithAutoReconnect(minDelay, maxDelay time.Duration, maxRetries int) Option {
	return func(c *config) {
		c.enableAutoReconnect = true
		c.minReconnectDelay = minDelay
		c.maxReconnectDelay = maxDelay
		c.maxReconnectRetries = maxRetries
	}
}

// Filter is a function that determines whether a container should be included.
type Filter func(Container) bool

// All returns a filter that matches if all given filters match (AND).
// Returns true if the filter list is empty.
func All(filters ...Filter) Filter {
	return func(c Container) bool {
		for _, filter := range filters {
			if !filter(c) {
				return false
			}
		}
		return true
	}
}

// Any returns a filter that matches if any given filter matches (OR).
// Returns false if the filter list is empty.
func Any(filters ...Filter) Filter {
	return func(c Container) bool {
		for _, filter := range filters {
			if filter(c) {
				return true
			}
		}
		return false
	}
}

// Not returns a filter that inverts the result of the given filter.
func Not(filter Filter) Filter {
	return func(c Container) bool {
		return !filter(c)
	}
}

// LabelExists returns a filter that matches containers with the given label key.
func LabelExists(key string) Filter {
	return func(c Container) bool {
		_, exists := c.Labels[key]
		return exists
	}
}

// LabelEquals returns a filter that matches containers where
// the given label equals the given value.
func LabelEquals(key, value string) Filter {
	return func(c Container) bool {
		return c.Labels[key] == value
	}
}

// StateEquals returns a filter that matches containers in the given state.
func StateEquals(state string) Filter {
	return func(c Container) bool {
		return c.State == state
	}
}
