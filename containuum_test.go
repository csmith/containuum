package containuum

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"testing/synctest"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockDockerClient is a test implementation of DockerClient (both EventMonitor and ContainerInspector)
type mockDockerClient struct {
	eventCh    chan events.Message
	errCh      chan error
	summaries  []container.Summary
	inspects   map[string]container.InspectResponse
	listErr    error
	inspectErr map[string]error
	listCalls  int
	// blocks makes inspects of an ID wait until the channel is closed.
	blocks      map[string]chan struct{}
	inflight    map[string]int
	maxInflight map[string]int
	cancelled   map[string]int
	mu          sync.Mutex
}

func newMockDockerClient() *mockDockerClient {
	return &mockDockerClient{
		eventCh:     make(chan events.Message, 10),
		errCh:       make(chan error, 10),
		inspects:    make(map[string]container.InspectResponse),
		inspectErr:  make(map[string]error),
		blocks:      make(map[string]chan struct{}),
		inflight:    make(map[string]int),
		maxInflight: make(map[string]int),
		cancelled:   make(map[string]int),
	}
}

func (m *mockDockerClient) Events(_ context.Context, _ client.EventsListOptions) client.EventsResult {
	return client.EventsResult{Messages: m.eventCh, Err: m.errCh}
}

func (m *mockDockerClient) ContainerList(_ context.Context, _ client.ContainerListOptions) (client.ContainerListResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.listCalls++
	if m.listErr != nil {
		return client.ContainerListResult{}, m.listErr
	}
	return client.ContainerListResult{Items: m.summaries}, nil
}

func (m *mockDockerClient) ContainerInspect(
	ctx context.Context,
	containerID string,
	_ client.ContainerInspectOptions,
) (client.ContainerInspectResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if block, ok := m.blocks[containerID]; ok {
		m.inflight[containerID]++
		m.maxInflight[containerID] = max(m.maxInflight[containerID], m.inflight[containerID])
		m.mu.Unlock()
		select {
		case <-block:
		case <-ctx.Done():
		}
		m.mu.Lock()
		m.inflight[containerID]--
		if ctx.Err() != nil {
			m.cancelled[containerID]++
			return client.ContainerInspectResult{}, ctx.Err()
		}
	}
	if err, ok := m.inspectErr[containerID]; ok {
		return client.ContainerInspectResult{}, err
	}
	if inspect, ok := m.inspects[containerID]; ok {
		return client.ContainerInspectResult{Container: inspect}, nil
	}
	return client.ContainerInspectResult{}, fmt.Errorf("container %s: %w", containerID, cerrdefs.ErrNotFound)
}

func (m *mockDockerClient) setInspectErr(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.inspectErr, id)
	} else {
		m.inspectErr[id] = err
	}
}

// block makes inspects of the container hang until unblock is called.
func (m *mockDockerClient) block(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blocks[id] = make(chan struct{})
}

func (m *mockDockerClient) unblock(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	close(m.blocks[id])
	delete(m.blocks, id)
}

func (m *mockDockerClient) stats(id string) (inflight, maxInflight, cancelled int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inflight[id], m.maxInflight[id], m.cancelled[id]
}

func (m *mockDockerClient) listCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.listCalls
}

func (m *mockDockerClient) removeInspect(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.inspects, id)
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
				ID:     "container1",
				Name:   "/test1",
				State:  &container.State{Status: "running"},
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
				ID:     "container1",
				Name:   "/test1",
				State:  &container.State{Status: "running"},
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
				ID:     "container1",
				Name:   "/test1-renamed",
				State:  &container.State{Status: "running"},
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
				ID:     "container1",
				Name:   "/running",
				State:  &container.State{Status: "running"},
				Config: &container.Config{Image: "nginx:latest"},
			},
			container.InspectResponse{
				ID:     "container2",
				Name:   "/exited",
				State:  &container.State{Status: "exited"},
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
				ID:     "container1",
				Name:   "/test1",
				State:  &container.State{Status: "running"},
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
				ID:     "container1",
				Name:   "/test1",
				State:  &container.State{Status: "running"},
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
				ID:     "container1",
				Name:   "/test1",
				State:  &container.State{Status: "exited"},
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
				ID:     "container1",
				Name:   "/test1",
				State:  &container.State{Status: "running"},
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
				ID:     "container1",
				Name:   "/test1",
				State:  &container.State{Status: "running"},
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
				ID:     "container1",
				Name:   "/test1",
				State:  &container.State{Status: "running"},
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

func testContainer(id, state string) container.InspectResponse {
	return container.InspectResponse{
		ID:     id,
		Name:   "/" + id,
		State:  &container.State{Status: container.ContainerState(state)},
		Config: &container.Config{Image: "nginx:latest"},
	}
}

// recorder runs the monitor against a mock client, recording each callback
// and each error passed to the error handler.
type recorder struct {
	mu     sync.Mutex
	calls  [][]Container
	errs   []error
	cancel context.CancelFunc
	errCh  chan error
	mock   *mockDockerClient
}

func startRecorder(t *testing.T, mock *mockDockerClient, opts ...Option) *recorder {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &recorder{cancel: cancel, errCh: make(chan error, 1), mock: mock}
	opts = append(
		[]Option{
			WithDockerClient(mock),
			WithDebounce(10 * time.Millisecond),
			WithErrorHandler(
				func(err error) {
					r.mu.Lock()
					defer r.mu.Unlock()
					r.errs = append(r.errs, err)
				},
			),
		},
		opts...,
	)
	go func() {
		r.errCh <- Run(
			ctx,
			func(containers []Container) {
				r.mu.Lock()
				defer r.mu.Unlock()
				r.calls = append(r.calls, containers)
			},
			opts...,
		)
	}()
	t.Cleanup(
		func() {
			cancel()
			synctest.Wait()
		},
	)
	time.Sleep(time.Millisecond)
	synctest.Wait()
	return r
}

// event sends an event and waits for the debounce to pass.
func (r *recorder) event() {
	r.mock.eventCh <- events.Message{Type: "container", Action: "start"}
	time.Sleep(50 * time.Millisecond)
	synctest.Wait()
}

func (r *recorder) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// last returns the containers from the latest callback as id => state.
func (r *recorder) last() map[string]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.calls) == 0 {
		return nil
	}
	res := make(map[string]string)
	for _, c := range r.calls[len(r.calls)-1] {
		res[c.ID] = c.State
	}
	return res
}

func (r *recorder) errorCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errs)
}

func TestRun_InspectFailureKeepsLastState(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)
			assert.Equal(t, 1, r.callCount())

			mock.setContainers(testContainer("a", "exited"), testContainer("b", "running"))
			mock.setInspectErr("a", errors.New("boom"))
			r.event()
			assert.Equal(t, 1, r.callCount(), "a failed inspect shouldn't change the state")
			assert.Equal(t, 1, r.errorCount())
			r.mu.Lock()
			require.ErrorContains(t, r.errs[0], "boom")
			require.ErrorContains(t, r.errs[0], "a")
			r.mu.Unlock()

			mock.setContainers(testContainer("a", "exited"), testContainer("b", "exited"))
			r.event()
			assert.Equal(t, 2, r.callCount())
			assert.Equal(t, map[string]string{"a": "running", "b": "exited"}, r.last())
			assert.Equal(t, 1, r.errorCount(), "repeated failures should only be reported once")

			mock.setInspectErr("a", nil)
			r.event()
			assert.Equal(t, map[string]string{"a": "exited", "b": "exited"}, r.last())

			mock.setInspectErr("a", errors.New("boom again"))
			r.event()
			assert.Equal(t, 2, r.errorCount(), "a new failure after a success should be reported")
		},
	)
}

func TestRun_InspectFailureOnNewContainer(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"))
			r := startRecorder(t, mock)

			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"), testContainer("c", "running"))
			mock.setInspectErr("b", errors.New("boom"))
			r.event()
			assert.Equal(t, map[string]string{"a": "running", "c": "running"}, r.last())
			assert.Equal(t, 1, r.errorCount())

			mock.setInspectErr("b", nil)
			r.event()
			assert.Equal(t, map[string]string{"a": "running", "b": "running", "c": "running"}, r.last())
		},
	)
}

func TestRun_InspectFailureOnInitialRefresh(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			mock.setInspectErr("a", errors.New("boom"))
			r := startRecorder(t, mock)

			select {
			case err := <-r.errCh:
				t.Fatalf("Run returned early: %v", err)
			default:
			}
			assert.Equal(t, map[string]string{"b": "running"}, r.last())
			assert.Equal(t, 1, r.errorCount())
		},
	)
}

func TestRun_InspectNotFoundDropsContainer(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.removeInspect("a")
			r.event()
			assert.Equal(t, map[string]string{"b": "running"}, r.last())
			assert.Equal(t, 0, r.errorCount())
		},
	)
}

func TestRun_UnlistedContainerIsForgotten(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.setContainers(testContainer("b", "running"))
			r.event()
			assert.Equal(t, map[string]string{"b": "running"}, r.last())

			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			mock.setInspectErr("a", errors.New("boom"))
			r.event()
			assert.Equal(t, 2, r.callCount(), "the forgotten state of a shouldn't come back")
			assert.Equal(t, map[string]string{"b": "running"}, r.last())
		},
	)
}

func TestRun_ListFailureAfterStart(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"))
			r := startRecorder(t, mock)

			mock.mu.Lock()
			mock.listErr = errors.New("docker api unreachable")
			mock.mu.Unlock()
			r.mock.eventCh <- events.Message{Type: "container", Action: "start"}

			err := <-r.errCh
			require.ErrorContains(t, err, "docker api unreachable")
		},
	)
}

// sleep advances the fake clock and lets everything settle.
func sleep(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func TestRun_HungInspectOnKnownContainer(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.block("a")
			mock.setContainers(testContainer("a", "exited"), testContainer("b", "exited"))
			mock.eventCh <- events.Message{Type: "container", Action: "start"}
			sleep(10*time.Millisecond + inspectStallTime - time.Millisecond)
			assert.Equal(t, 1, r.callCount(), "shouldn't emit before the stall time")

			sleep(time.Millisecond)
			assert.Equal(t, 2, r.callCount())
			assert.Equal(t, map[string]string{"a": "running", "b": "exited"}, r.last())
			assert.Equal(t, 1, r.errorCount())

			mock.unblock("a")
			sleep(50 * time.Millisecond)
			assert.Equal(t, 3, r.callCount())
			assert.Equal(t, map[string]string{"a": "exited", "b": "exited"}, r.last())
			assert.Equal(t, 1, r.errorCount())
		},
	)
}

func TestRun_HungInspectOnNewContainer(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"))
			r := startRecorder(t, mock)

			mock.block("b")
			mock.setContainers(testContainer("a", "exited"), testContainer("b", "running"))
			mock.eventCh <- events.Message{Type: "container", Action: "start"}
			sleep(10*time.Millisecond + inspectStallTime)
			assert.Equal(t, map[string]string{"a": "exited"}, r.last())

			mock.unblock("b")
			sleep(50 * time.Millisecond)
			assert.Equal(t, map[string]string{"a": "exited", "b": "running"}, r.last())
		},
	)
}

func TestRun_HungInspectOnFirstRefresh(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			mock.block("a")
			r := startRecorder(t, mock)

			sleep(initialInspectStallTime - 2*time.Millisecond)
			assert.Equal(t, 0, r.callCount(), "the first refresh should wait longer")

			sleep(time.Millisecond)
			assert.Equal(t, 1, r.callCount())
			assert.Equal(t, map[string]string{"b": "running"}, r.last())

			mock.unblock("a")
			sleep(50 * time.Millisecond)
			assert.Equal(t, map[string]string{"a": "running", "b": "running"}, r.last())
		},
	)
}

func TestRun_MoreHungInspectsThanConcurrencyLimit(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			var all []container.InspectResponse
			for i := range maxConcurrentInspects + 4 {
				all = append(all, testContainer(fmt.Sprintf("hung%d", i), "running"))
			}
			healthy := testContainer("healthy", "running")

			mock := newMockDockerClient()
			mock.setContainers(append(all, healthy)...)
			r := startRecorder(t, mock)

			for _, c := range all {
				mock.block(c.ID)
			}
			mock.setContainers(append(all, testContainer("healthy", "exited"))...)
			mock.eventCh <- events.Message{Type: "container", Action: "start"}
			sleep(10*time.Millisecond + inspectStallTime + 50*time.Millisecond)

			assert.Equal(t, "exited", r.last()["healthy"])
			assert.Len(t, r.last(), len(all)+1)
			_, maxInflight, _ := mock.stats("hung0")
			assert.Equal(t, 1, maxInflight)
		},
	)
}

func TestRun_StuckContainerHasOneInspectInFlight(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.block("a")
			r.event()
			sleep(inspectStallTime)

			for _, state := range []string{"exited", "running", "exited"} {
				mock.setContainers(testContainer("a", "running"), testContainer("b", state))
				r.event()
				assert.Equal(t, state, r.last()["b"], "later refreshes shouldn't wait for a")
			}

			inflight, maxInflight, _ := mock.stats("a")
			assert.Equal(t, 1, inflight)
			assert.Equal(t, 1, maxInflight)
			assert.Equal(t, 1, r.errorCount())
		},
	)
}

func TestRun_HungInspectThatFailsDoesNotRefresh(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.block("a")
			r.event()
			sleep(inspectStallTime)
			calls, lists := r.callCount(), mock.listCount()

			mock.setInspectErr("a", errors.New("boom"))
			mock.unblock("a")
			sleep(time.Second)

			assert.Equal(t, lists, mock.listCount(), "a failure shouldn't trigger a refresh")
			assert.Equal(t, calls, r.callCount())
			assert.Equal(t, 1, r.errorCount())
		},
	)
}

func TestRun_SuccessAfterRefreshSchedulesAnother(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.block("a")
			r.event()
			sleep(inspectStallTime)
			r.event()
			lists := mock.listCount()

			mock.unblock("a")
			sleep(50 * time.Millisecond)
			assert.Equal(t, lists+1, mock.listCount())
		},
	)
}

func TestRun_RemovedWhileHungIsCancelled(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.block("a")
			r.event()
			sleep(inspectStallTime)

			mock.setContainers(testContainer("b", "running"))
			r.event()
			assert.Equal(t, map[string]string{"b": "running"}, r.last())

			inflight, _, cancelled := mock.stats("a")
			assert.Equal(t, 0, inflight)
			assert.Equal(t, 1, cancelled)
		},
	)
}

func TestRun_CancelsInspectsOnReturn(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.block("a")
			r.event()
			sleep(inspectStallTime)

			mock.errCh <- errors.New("stream closed")
			require.ErrorContains(t, <-r.errCh, "stream closed")

			inflight, _, cancelled := mock.stats("a")
			assert.Equal(t, 0, inflight)
			assert.Equal(t, 1, cancelled)
		},
	)
}

func TestRun_InspectResultSurvivesReconnect(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock, WithAutoReconnect(time.Second, time.Second, 0))

			mock.block("a")
			mock.setContainers(testContainer("a", "exited"), testContainer("b", "running"))
			r.event()
			sleep(inspectStallTime)

			mock.errCh <- errors.New("stream closed")
			sleep(100 * time.Millisecond)
			mock.unblock("a")
			sleep(time.Second)

			assert.Equal(t, map[string]string{"a": "exited", "b": "running"}, r.last())
			_, maxInflight, _ := mock.stats("a")
			assert.Equal(t, 1, maxInflight)
		},
	)
}

func TestRun_FailingContainerIsNotWaitedFor(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"), testContainer("b", "running"))
			r := startRecorder(t, mock)

			mock.setInspectErr("a", errors.New("boom"))
			r.event()
			assert.Equal(t, 1, r.errorCount())

			mock.block("a")
			mock.setContainers(testContainer("a", "running"), testContainer("b", "exited"))
			r.event()
			assert.Equal(t, map[string]string{"a": "running", "b": "exited"}, r.last())
		},
	)
}

func TestRun_LateInspectResultsAreDebounced(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			running := make([]container.InspectResponse, 0, 10)
			exited := make([]container.InspectResponse, 0, 10)
			for i := range 10 {
				running = append(running, testContainer(fmt.Sprintf("c%d", i), "running"))
				exited = append(exited, testContainer(fmt.Sprintf("c%d", i), "exited"))
			}

			mock := newMockDockerClient()
			mock.setContainers(running...)
			r := startRecorder(t, mock)

			for _, c := range running {
				mock.setInspectErr(c.ID, errors.New("boom"))
			}
			r.event()
			assert.Equal(t, 10, r.errorCount())
			calls := r.callCount()

			for _, c := range running {
				mock.setInspectErr(c.ID, nil)
			}
			mock.setContainers(exited...)
			r.event()

			assert.Equal(t, calls+1, r.callCount(), "late results should be coalesced into one callback")
			for _, state := range r.last() {
				assert.Equal(t, "exited", state)
			}
			assert.Len(t, r.last(), 10)
		},
	)
}

func TestRun_CallbackCannotMutateStoredState(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			c := testContainer("a", "running")
			c.Config.Labels = map[string]string{"keep": "1", "drop": "1"}
			c.NetworkSettings = &container.NetworkSettings{
				Networks: map[string]*network.EndpointSettings{
					"net": {Aliases: []string{"a"}},
				},
			}
			mock := newMockDockerClient()
			mock.setContainers(c, testContainer("b", "running"))

			var calls [][]Container
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				_ = Run(
					ctx,
					func(containers []Container) {
						calls = append(calls, containers)
						if len(calls) > 1 {
							return
						}
						for i := range containers {
							delete(containers[i].Labels, "drop")
							for j := range containers[i].Networks {
								containers[i].Networks[j].Aliases[0] = "mutated"
							}
						}
					},
					WithDockerClient(mock),
					WithDebounce(10*time.Millisecond),
				)
			}()
			sleep(time.Millisecond)

			mock.setInspectErr("a", errors.New("boom"))
			mock.setContainers(c, testContainer("b", "exited"))
			mock.eventCh <- events.Message{Type: "container", Action: "start"}
			sleep(50 * time.Millisecond)

			require.Len(t, calls, 2)
			assert.Equal(t, map[string]string{"keep": "1", "drop": "1"}, calls[1][0].Labels)
			assert.Equal(t, []string{"a"}, calls[1][0].Networks[0].Aliases)
		},
	)
}

func TestRun_ReconnectErrorsGoToErrorHandler(t *testing.T) {
	synctest.Test(
		t,
		func(t *testing.T) {
			mock := newMockDockerClient()
			mock.setContainers(testContainer("a", "running"))
			r := startRecorder(t, mock, WithAutoReconnect(time.Second, time.Second, 0))

			mock.errCh <- errors.New("stream closed")
			sleep(10 * time.Millisecond)
			require.Equal(t, 1, r.errorCount())

			mock.mu.Lock()
			mock.listErr = errors.New("docker api unreachable")
			mock.mu.Unlock()
			sleep(time.Second)
			require.Equal(t, 2, r.errorCount())

			r.mu.Lock()
			defer r.mu.Unlock()
			require.ErrorContains(t, r.errs[0], "stream closed")
			require.ErrorContains(t, r.errs[1], "docker api unreachable")
		},
	)
}
