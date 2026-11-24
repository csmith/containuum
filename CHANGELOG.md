# Changelog

## Unreleased

- Update OpenTelemetry to fix GO-2026-6974; Go 1.26 is now required

## 3.1.0 - 2026-11-16

- Add `WithErrorHandler` to receive inspect and reconnect errors that don't
  stop monitoring
- Fix containers being dropped from the list when they fail to be inspected;
  they now keep their last known state
- Fix one hung container delaying every refresh

## 3.0.0 - 2026-11-04

- **Breaking:** `DockerClient` now uses the types from
  `github.com/moby/moby/client`, replacing `github.com/docker/docker`.

## 2.0.0 - 2026-09-06

- **Breaking:** `Container.Ports` now includes exposed-but-unpublished ports
  with HostPort set to `0` and an empty `HostIP`.

## 1.0.0 - 2025-12-21

_Initial version._
