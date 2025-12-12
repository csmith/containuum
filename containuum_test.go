package containuum

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"testing/synctest"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/stretchr/testify/assert"
)

// mockDockerClient is a test implementation of DockerClient (both EventMonitor and ContainerInspector)
type mockDockerClient struct {
	eventCh    chan events.Message
	errCh      chan error
	summaries  []container.Summary
	inspects   map[string]container.InspectResponse
	listErr    error
	inspectErr map[string]error
	mu         sync.Mutex
}

func newMockDockerClient() *mockDockerClient {
	return &mockDockerClient{
		eventCh:    make(chan events.Message, 10),
		errCh:      make(chan error, 10),
		inspects:   make(map[string]container.InspectResponse),
		inspectErr: make(map[string]error),
	}
}

func (m *mockDockerClient) Events(_ context.Context, _ events.ListOptions) (<-chan events.Message, <-chan error) {
	return m.eventCh, m.errCh
}

func (m *mockDockerClient) ContainerList(_ context.Context, _ container.ListOptions) ([]container.Summary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.summaries, nil
}

func (m *mockDockerClient) ContainerInspect(_ context.Context, containerID string) (container.InspectResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err, ok := m.inspectErr[containerID]; ok {
		return container.InspectResponse{}, err
	}
	if inspect, ok := m.inspects[containerID]; ok {
		return inspect, nil
	}
	return container.InspectResponse{}, fmt.Errorf("container not found: %s", containerID)
}

func (m *mockDockerClient) setContainers(containers ...container.InspectResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.summaries = nil
	for _, c := range containers {
		m.summaries = append(m.summaries, container.Summary{ID: c.ID})
		m.inspects[c.ID] = c
	}
}

func TestRun_ReceivesInitialContainers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		var callbackCalled bool
		var receivedContainers []Container

		callback := func(containers []Container) {
			callbackCalled = true
			receivedContainers = containers
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback, WithDockerClient(mock))
		}()

		// Wait for initial callback
		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		assert.True(t, callbackCalled, "callback should have been called")
		assert.Len(t, receivedContainers, 1)
		assert.Equal(t, "container1", receivedContainers[0].ID)
		assert.Equal(t, "test1", receivedContainers[0].Name)

		cancel()
		err := <-errCh
		assert.Equal(t, context.Canceled, err)
	})
}

func TestRun_MultipleEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		callCount := 0
		mu := sync.Mutex{}

		callback := func(containers []Container) {
			mu.Lock()
			callCount++
			mu.Unlock()
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback, WithDockerClient(mock), WithDebounce(100*time.Millisecond))
		}()

		// Wait for initial callback
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		initialCount := callCount
		mu.Unlock()
		assert.Equal(t, 1, initialCount)

		// Change container state so it's not deduplicated
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1-renamed",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		// Send an event
		mock.eventCh <- events.Message{Type: "container", Action: "start"}
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		afterEventCount := callCount
		mu.Unlock()
		assert.Equal(t, 2, afterEventCount)

		cancel()
		<-errCh
	})
}

func TestRun_AppliesFilter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/running",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container2",
					Name:  "/exited",
					State: &container.State{Status: "exited"},
				},
				Config: &container.Config{Image: "redis:latest"},
			},
		)

		var receivedContainers []Container

		callback := func(containers []Container) {
			receivedContainers = containers
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback,
				WithDockerClient(mock),
				WithFilter(StateEquals("running")),
			)
		}()

		time.Sleep(100 * time.Millisecond)
		synctest.Wait()

		assert.Len(t, receivedContainers, 1)
		assert.Equal(t, "container1", receivedContainers[0].ID)

		cancel()
		<-errCh
	})
}

func TestRun_Deduplicates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		callCount := 0
		mu := sync.Mutex{}

		callback := func(containers []Container) {
			mu.Lock()
			callCount++
			mu.Unlock()
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback, WithDockerClient(mock), WithDebounce(10*time.Millisecond))
		}()

		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		initialCount := callCount
		mu.Unlock()
		assert.Equal(t, 1, initialCount)

		// Send an event but don't change container state
		mock.eventCh <- events.Message{Type: "container", Action: "start"}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		afterEventCount := callCount
		mu.Unlock()
		// Should still be 1 because state didn't change
		assert.Equal(t, 1, afterEventCount)

		cancel()
		<-errCh
	})
}

func TestRun_StateChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		callCount := 0
		var lastContainers []Container
		mu := sync.Mutex{}

		callback := func(containers []Container) {
			mu.Lock()
			callCount++
			lastContainers = containers
			mu.Unlock()
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback, WithDockerClient(mock), WithDebounce(10*time.Millisecond))
		}()

		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		// Change container state
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1",
					State: &container.State{Status: "exited"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		// Trigger event
		mock.eventCh <- events.Message{Type: "container", Action: "stop"}
		time.Sleep(50 * time.Millisecond)
		synctest.Wait()

		mu.Lock()
		assert.Equal(t, 2, callCount)
		assert.Equal(t, "exited", lastContainers[0].State)
		mu.Unlock()

		cancel()
		<-errCh
	})
}

func TestRun_WatcherError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		mock := newMockDockerClient()
		mock.setContainers()

		callback := func(containers []Container) {}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback, WithDockerClient(mock))
		}()

		// Send error from watcher
		testErr := fmt.Errorf("watcher connection lost")
		mock.errCh <- testErr

		// Wait for error to propagate
		err := <-errCh
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "watcher connection lost")

		cancel()
		synctest.Wait()
	})
}

func TestRun_GathererError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()
		mock.listErr = fmt.Errorf("docker api unreachable")

		callback := func(containers []Container) {}

		err := Run(ctx, callback, WithDockerClient(mock))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "docker api unreachable")
	})
}

func TestRun_ContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		mock := newMockDockerClient()
		mock.setContainers()

		callback := func(containers []Container) {}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback, WithDockerClient(mock))
		}()

		time.Sleep(50 * time.Millisecond)
		cancel()

		synctest.Wait()

		err := <-errCh
		assert.Equal(t, context.Canceled, err)
	})
}

func TestRun_WithDebounceOption(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()

		callbackCh := make(chan struct{}, 1)
		callback := func(containers []Container) {
			slog.Info("Callback")
			callbackCh <- struct{}{}
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback,
				WithDockerClient(mock),
				WithDebounce(5*time.Second),
			)
		}()

		// Initial callback
		<-callbackCh

		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		mock.eventCh <- events.Message{Type: "container", Action: "start"}

		start := time.Now()

		// Wait for second callback (after debounce)
		<-callbackCh

		elapsed := time.Since(start)

		// Should have waited for debounce period
		assert.Equal(t, 5*time.Second, elapsed)

		cancel()
		<-errCh
	})
}

func TestRun_WithMaxDebounceTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		callbackCh := make(chan struct{}, 1)

		callback := func(containers []Container) {
			select {
			case callbackCh <- struct{}{}:
			default:
			}
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback,
				WithDockerClient(mock),
				WithDebounce(10*time.Second),
				WithMaxDebounceTime(2*time.Second),
			)
		}()

		start := time.Now()

		// Spam events to keep debounce from expiring
		// Events every 100ms for 2 seconds will keep resetting the 10s debounce
		// but max wait will trigger at 2s
		for i := 0; i < 20; i++ {
			mock.eventCh <- events.Message{Type: "container", Action: "start"}
			time.Sleep(100 * time.Millisecond)
		}

		// Wait for callback (triggered by max wait)
		<-callbackCh

		elapsed := time.Since(start)

		// Should have emitted at max wait time, not debounce time
		assert.Equal(t, 2*time.Second, elapsed)

		cancel()
		<-errCh
	})
}

func TestRun_WithMaxIdleTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		mock := newMockDockerClient()

		callbackCh := make(chan struct{}, 10)

		callback := func(containers []Container) {
			callbackCh <- struct{}{}
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- Run(ctx, callback,
				WithDockerClient(mock),
				WithDebounce(10*time.Millisecond),
				WithMaxIdleTime(1*time.Second),
			)
		}()

		// Wait for initial callback
		<-callbackCh

		// Set containers so next idle poll isn't deduplicated
		mock.setContainers(
			container.InspectResponse{
				ContainerJSONBase: &container.ContainerJSONBase{
					ID:    "container1",
					Name:  "/test1",
					State: &container.State{Status: "running"},
				},
				Config: &container.Config{Image: "nginx:latest"},
			},
		)

		start := time.Now()

		// Wait for idle timer to trigger
		<-callbackCh
		elapsed := time.Since(start)

		// Should happen after max idle time
		assert.Equal(t, 1*time.Second, elapsed)

		cancel()
		<-errCh
	})
}
