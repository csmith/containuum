//go:build integration

package containuum_test

import (
	"context"
	"encoding/json"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/csmith/containuum"
	"github.com/sebdah/goldie/v2"
	"github.com/stretchr/testify/require"
)

func TestIntegration_Basic(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var mu sync.Mutex
	var callbacks [][]containuum.Container

	callback := func(containers []containuum.Container) {
		mu.Lock()
		callbacks = append(callbacks, containers)
		mu.Unlock()
		t.Logf("Callback #%d: %d containers", len(callbacks), len(containers))
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- containuum.Run(ctx, callback,
			containuum.WithFilter(containuum.All(
				containuum.LabelEquals("containuum.test", "true"),
				containuum.StateEquals("running"),
			)),
			containuum.WithDebounce(50*time.Millisecond),
			containuum.WithMaxDebounceTime(200*time.Millisecond),
			containuum.WithMaxIdleTime(10*time.Second),
		)
	}()

	time.Sleep(500 * time.Millisecond)

	cmd := exec.Command("./testdata/basic.sh")
	output, err := cmd.CombinedOutput()
	t.Logf("Script output:\n%s", output)
	require.NoError(t, err, "script failed")

	time.Sleep(1 * time.Second)
	cancel()

	err = <-errCh
	require.ErrorIs(t, err, context.Canceled)

	mu.Lock()
	defer mu.Unlock()

	// Simplify to just essential fields for testing
	type simpleContainer struct {
		Name  string
		Image string
		State string
	}

	simplified := make([][]simpleContainer, len(callbacks))
	for i, cb := range callbacks {
		simplified[i] = make([]simpleContainer, len(cb))
		for j, c := range cb {
			simplified[i][j] = simpleContainer{
				Name:  c.Name,
				Image: c.Image,
				State: c.State,
			}
		}
	}

	data, err := json.MarshalIndent(simplified, "", "  ")
	require.NoError(t, err)

	g := goldie.New(t)
	g.Assert(t, "basic", data)
}
