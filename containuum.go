package containuum

import (
	"context"
	"fmt"

	"github.com/docker/docker/client"
)

// Run monitors Docker containers and calls the callback when the filtered set changes.
// It emits the initial state immediately, then watches for changes.
// Blocks until the context is cancelled or an error occurs.
func Run(ctx context.Context, callback Callback, opts ...Option) error {
	cfg := defaultConfig()
	for _, opt := range opts {
		opt(cfg)
	}

	dockerClient := cfg.client
	if dockerClient == nil {
		var cleanup func() error
		var err error
		dockerClient, cleanup, err = newDefaultClient()
		if err != nil {
			return err
		}
		defer func() { _ = cleanup() }()
	}

	var reconnect *reconnectConfig
	if cfg.enableAutoReconnect {
		reconnect = &reconnectConfig{
			MinDelay:   cfg.minReconnectDelay,
			MaxDelay:   cfg.maxReconnectDelay,
			MaxRetries: cfg.maxReconnectRetries,
		}
	}

	m := &monitor{
		ctx:             ctx,
		client:          dockerClient,
		callback:        callback,
		filter:          cfg.filter,
		debounce:        cfg.debounce,
		maxDebounceTime: cfg.maxDebounceTime,
		maxIdleTime:     cfg.maxIdleTime,
		reconnect:       reconnect,
	}

	Log("entering main event loop")
	return m.run()
}

// newDefaultClient creates a default Docker client from the environment.
// Returns the client and a cleanup function that should be called when done.
func newDefaultClient() (DockerClient, func() error, error) {
	c, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create docker client: %w", err)
	}
	return c, c.Close, nil
}
