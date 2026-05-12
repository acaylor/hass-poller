# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Added `.github/workflows/release.yml`: on push of a `v*.*.*` tag, extracts the matching `CHANGELOG.md` section and creates a GitHub release. The workflow is also compatible with Gitea Actions (Gitea reads `.github/workflows/`, and `softprops/action-gh-release` honors `$GITHUB_API_URL`).
- Added `.github/workflows/test.yml`: on push to `main` and on every pull request, runs `go mod verify`, `go vet`, and `go test -race ./...` against the Go toolchain pinned in `go.mod`.
- Added unit tests for `internal/config` (now at 100% coverage of statements) and for `engine.epsilonFor`.
- Added `internal/ha/client_test.go` covering auth header, base-URL normalization, non-200, malformed JSON, context cancellation, and transport errors. `internal/ha` is now at 100% coverage.
- Added `internal/httpserver/server_test.go` covering `/healthz` across healthy/stale/never-polled/db-down states, the `/metrics` endpoint, `Shutdown`, and `AtomicTime`. `internal/httpserver` is now at 100% coverage.
- Added `internal/engine/fakes_test.go` with hand-written fakes for the new interfaces, plus cycle tests covering filtering, numeric parsing, epsilon skip, per-entity overrides, fetch and insert error propagation, concurrent-cycle bail-out, ticker-driven re-entry, and `Run` context cancellation. `internal/engine` is now at 100% coverage. Total project coverage on the testable packages rose from ~33.6% to ~98%.
- Added `.env.example` documenting every variable the bundled docker-compose stack reads.

### Changed

- Refactored `internal/engine` to depend on `StatesFetcher` and `MeasurementStore` interfaces rather than the concrete `*ha.Client` and `*store.Store` types. Production wiring is unchanged because the concrete types already satisfy the interfaces; the refactor exists to enable hand-written fakes in unit tests.
- Refactored `docker-compose.yml` so every tunable (Postgres credentials, blocklist, host ports, poll interval, log level) is supplied via `.env` rather than hard-coded. The file is now a generic working dev stack; required variables fail-fast with a clear message if missing.
- Removed the deployment-specific default `ENTITY_BLOCKLIST` that named individual circuits on the author's home setup. The blocklist now defaults to empty; see `.env.example` for representative patterns.

### Removed

- Removed `code-plan.md`, the pre-implementation design draft. `docs/ARCHITECTURE.md` is the maintained replacement.

## [0.1.0] - 2026-05-05

### Added

- Added an MIT `LICENSE` file for the public release.
- Added this `CHANGELOG.md` to track notable changes going forward.
- Added a daily continuous aggregate `ha_numeric_1d` (`avg`/`min`/`max`/`count` per entity per day) with no retention policy, so long-range historical data is retained forever at daily resolution.
- Added a 1-year retention policy on the hourly continuous aggregate `ha_numeric_1h` so it does not grow unboundedly.
- Documented how to take and restore `pg_dump` backups; added `backups/` to `.gitignore`.
- Added a `CONTRIBUTING.md` covering branch naming (`<type>/<kebab-description>`), commit style, PR workflow, and the release process.
- Added [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) describing components, data flow, the tiered schema, filtering, change detection, and failure modes.
- Added [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) covering build, run, test, and diagram regeneration.
- Added the d2 architecture diagram source (`docs/diagrams/architecture.d2`) and rendered SVG (`docs/diagrams/d2.svg`).

### Changed

- Expanded the default `ENTITY_BLOCKLIST` in `docker-compose.yml` to drop redundant or low-signal sensors at ingestion time:
  - `*_power_minute_average` (sliding-window averages already derivable from energy counters)
  - `*_signal_level`, `*_disk_write_rate`, `*_storage_used` (device-health metrics not relevant to home energy/climate analytics)
  - Specific phantom circuits and duplicate appliance sensors from the author's local deployment.
- Slimmed down `README.md` — moved development instructions to `docs/DEVELOPMENT.md` and architecture/schema details to `docs/ARCHITECTURE.md`.

### Removed

- Removed the redundant Mermaid architecture diagram (`docs/diagrams/architecture.mmd` and its rendered SVG); the d2 version is now the canonical source.
