# Containuum

Containuum is a library for subscribing to changes in container state. It
monitors the event stream from docker, and emits deduplicated lists of
containers to subscribers. It's intended to be used by tools that automatically
generate config files based on container labels (e.g. for a reverse proxy),
or perform other updates when a container starts/stops (e.g. adding DNS entries).

## Basic usage

Containuum exposes a "Run" function which takes a context, a callback, and
options. The Run function blocks until it fails or the context is cancelled.

```go
package main

import (
	"context"
	"log/slog"

	"github.com/csmith/containuum"
)

func main() {
	err := containuum.Run(
		context.Background(),
		func(containers []containuum.Container) {
			slog.Info("Received containers", "count", len(containers))
		},
		containuum.WithFilter(containuum.LabelExists("web")),
	)
	if err != nil {
		slog.Error("Failed to monitor containers", "err", err)
	}
}
```

## Options

The following options can be passed to Containuum:

- `WithDockerClient` provides a specific Docker client to use. If not specified,
  one is created with default values. You can customise the behaviour of the
  default Docker client using [env vars](https://pkg.go.dev/github.com/docker/docker/client#FromEnv).
- `WithFilter` applies a filter to containers that are returned. See the
  filters section below. Only one top-level filter may be applied.
- `WithDebounce` configures the debounce on incoming container events. This
  can reduce how often the callback is invoked on exceptionally busy systems
  or when a container is misbehaving. Default: `100ms`
- `WithMaxDebounceTime` configures the maximum time events will be debounced
  for. This ensures that a constant stream of events emits updates at some
  point, rather than effectively becoming a denial-of-service attack.
  Default: `5s`.
- `WithMaxIdleTime` configures the period at which Continuum will refresh
  the containers even if it hasn't received an event. This is a useful fallback
  in case the event stream silently fails. Default: `30s`.
- `WithAutoReconnect` configures automatic reconnection to the event stream.
  If not specified, Containuum will error if the stream is disconnected, and
  clients must call `Run()` again to resume. Reconnection is performed with an
  exponential back-off, up to a maximum time limit.

## Filters

If you are only interested in a subset of containers, you can filter them
out by passing a filter to the `WithFilter` option. This occurs before
deduplication, ensuring that changes to other containers don't result in your
callback being invoked. The following filters are available:

- `Any(filter, filter, ...)` - matches containers that matches any of the given filters
- `All(filter, filter, ...)` - matches containers that matches all of the given filters
- `Not(filter)` - matches containers that do not match the given filter
- `LabelExists(string)` - matches containers that have the specified label, with any value
- `LabelEquals(string, string)` - matches containers that have the specified label with the specified value
- `StateEquals(string)` - matches contains in the given state (`running`, `stopped`, etc)

Only a single filter may be passed to `WithFilter`, but you can build complex
filter chains using `Any` and/or `All` as required.

## Provenance

This project was primarily created with Claude Code, but with a strong guiding
hand. It's not "vibe coded", but an LLM was still the primary author of most
lines of code. I believe it meets the same sort of standards I'd aim for with
hand-crafted code, but some slop may slip through. I understand if you
prefer not to use LLM-created software, and welcome human-authored alternatives
(I just don't personally have the time/motivation to do so).