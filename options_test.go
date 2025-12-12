package containuum

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

var (
	runningProdWeb = Container{
		ID:     "1",
		State:  "running",
		Labels: map[string]string{"app": "web", "env": "prod"},
	}

	exitedDevAPI = Container{
		ID:     "2",
		State:  "exited",
		Labels: map[string]string{"app": "api", "env": "dev"},
	}

	pausedStagingDB = Container{
		ID:     "3",
		State:  "paused",
		Labels: map[string]string{"app": "db", "env": "staging"},
	}

	runningNoLabels = Container{
		ID:     "4",
		State:  "running",
		Labels: map[string]string{},
	}
)

func TestFilters(t *testing.T) {
	tests := []struct {
		name      string
		filter    Filter
		container Container
		want      bool
	}{
		// All() tests
		{
			name:      "All() empty matches everything",
			filter:    All(),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "All() with single passing filter",
			filter:    All(StateEquals("running")),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "All() with single failing filter",
			filter:    All(StateEquals("exited")),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "All() with multiple passing filters",
			filter:    All(StateEquals("running"), LabelExists("app"), LabelEquals("env", "prod")),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "All() with one failing filter",
			filter:    All(StateEquals("running"), LabelEquals("env", "dev")),
			container: runningProdWeb,
			want:      false,
		},

		// Any() tests
		{
			name:      "Any() empty matches nothing",
			filter:    Any(),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "Any() with single passing filter",
			filter:    Any(StateEquals("running")),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "Any() with single failing filter",
			filter:    Any(StateEquals("exited")),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "Any() with first filter passing",
			filter:    Any(StateEquals("running"), StateEquals("paused")),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "Any() with last filter passing",
			filter:    Any(StateEquals("paused"), StateEquals("running")),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "Any() with all filters failing",
			filter:    Any(StateEquals("exited"), LabelEquals("env", "dev")),
			container: runningProdWeb,
			want:      false,
		},

		// Not() tests
		{
			name:      "Not() inverts passing filter",
			filter:    Not(StateEquals("running")),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "Not() inverts failing filter",
			filter:    Not(StateEquals("exited")),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "Not(LabelExists()) matches when label missing",
			filter:    Not(LabelExists("missing")),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "Not(LabelExists()) fails when label exists",
			filter:    Not(LabelExists("app")),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "Not(All(...)) inverts complex filter",
			filter:    Not(All(StateEquals("running"), LabelEquals("env", "prod"))),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "Not(All(...)) passes when inner fails",
			filter:    Not(All(StateEquals("running"), LabelEquals("env", "dev"))),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "Not(Any(...)) inverts or filter",
			filter:    Not(Any(StateEquals("exited"), StateEquals("paused"))),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "Not(Not(...)) double negation",
			filter:    Not(Not(StateEquals("running"))),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "All(Not(...), ...) negation in composition",
			filter:    All(StateEquals("running"), Not(LabelEquals("env", "dev"))),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "All(Not(...), ...) negation causes failure",
			filter:    All(StateEquals("running"), Not(LabelEquals("env", "prod"))),
			container: runningProdWeb,
			want:      false,
		},

		// LabelExists() tests
		{
			name:      "LabelExists() with existing label",
			filter:    LabelExists("app"),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "LabelExists() with missing label",
			filter:    LabelExists("missing"),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "LabelExists() with empty labels",
			filter:    LabelExists("app"),
			container: runningNoLabels,
			want:      false,
		},

		// LabelEquals() tests
		{
			name:      "LabelEquals() exact match",
			filter:    LabelEquals("env", "prod"),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "LabelEquals() wrong value",
			filter:    LabelEquals("env", "dev"),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "LabelEquals() missing label",
			filter:    LabelEquals("missing", "value"),
			container: runningProdWeb,
			want:      false,
		},

		// StateEquals() tests
		{
			name:      "StateEquals() matches",
			filter:    StateEquals("running"),
			container: runningProdWeb,
			want:      true,
		},
		{
			name:      "StateEquals() doesn't match",
			filter:    StateEquals("exited"),
			container: runningProdWeb,
			want:      false,
		},

		// Nested filters
		{
			name: "All(Any(...), Any(...)) complex nesting",
			filter: All(
				Any(StateEquals("running"), StateEquals("paused")),
				Any(LabelEquals("env", "prod"), LabelEquals("env", "staging")),
			),
			container: runningProdWeb,
			want:      true,
		},
		{
			name: "All(Any(...), Any(...)) first Any fails",
			filter: All(
				Any(StateEquals("exited"), StateEquals("paused")),
				Any(LabelEquals("env", "prod"), LabelEquals("env", "staging")),
			),
			container: runningProdWeb,
			want:      false,
		},
		{
			name: "All(Any(...), Any(...)) second Any fails",
			filter: All(
				Any(StateEquals("running"), StateEquals("paused")),
				Any(LabelEquals("env", "dev"), LabelEquals("env", "staging")),
			),
			container: runningProdWeb,
			want:      false,
		},
		{
			name:      "All() matches exited dev container",
			filter:    All(StateEquals("exited"), LabelEquals("env", "dev")),
			container: exitedDevAPI,
			want:      true,
		},
		{
			name:      "Any() matches when first label matches",
			filter:    Any(LabelEquals("app", "db"), LabelEquals("app", "cache")),
			container: pausedStagingDB,
			want:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.filter(tt.container)
			assert.Equal(t, tt.want, got)
		})
	}
}
