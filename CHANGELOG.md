# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- Keep the last-written cache unchanged on failed database inserts so later polls retry.
- Record unit changes even when the numeric value is unchanged, including removal of a unit.
- Make backfill replacement transactional and limit deletion to entities with replacement rows.
- Refresh complete hourly and daily buckets at backfill boundaries.
- Preserve fractional history request timestamps; reject empty history entity filters.
- Ignore out-of-range history before epsilon comparison and suppress duplicate boundary records.
- Scale health freshness to the poll interval, with a two-minute minimum and
  overflow protection for very large durations.
- Reject invalid durations, epsilon values, and glob patterns instead of silently using defaults.
- Keep the default allowlist when a CSV setting contains only commas and whitespace.
- Make the entities-seen gauge count all fetched entities, as its description promises.

### Changed

- Remove the redundant poll overlap mutex, custom atomic-time wrapper, and tests of wrapper behavior.
- Share streaming row encoding between normal and replacement inserts.
- Share unit-aware epsilon decisions between polling and backfill.
- Cancel backfill on SIGINT/SIGTERM and defer database setup until a write is needed.
- Remove the unimplemented `LOG_LEVEL` setting and correct shutdown documentation.

## [0.2.1] - 2026-09-04

### Security

- Updated the Go toolchain and Docker builder image from 1.26.4 to 1.27.1,
  resolving seven reachable standard-library vulnerabilities reported by
  `govulncheck`.
- Updated `github.com/prometheus/client_golang` from 1.23.2 to 1.24.1 and
  refreshed its transitive dependencies, including `golang.org/x/text` from
  0.29.0 to 0.40.0, resolving GO-2026-5970.

### Changed

- Updated `github.com/jackc/pgx/v5` from 5.9.2 to 5.10.0.
- Updated the bundled TimescaleDB image from 2.26.4 to 2.29.2.
- Updated the bundled Adminer image from 5.4.2 to 6.0.1.
- Updated `actions/checkout` and `actions/setup-go` to v7.

## [0.2.0] - 2026-07-06

### Added

- `backfill` command (`cmd/backfill`) to recover missing data after an outage by
  replaying Home Assistant recorder history into `ha_numeric`, then refreshing
  the hourly and daily continuous aggregates for the range. Reuses the poller's
  config, filtering, numeric parsing, and epsilon change-detection. Supports
  `-dry-run`, `-replace`, `-no-epsilon`, and `-no-refresh`.
- `docs/BACKFILL.md` runbook covering gap identification, retention limits,
  caveats, and running the command from source, Docker, or a Kubernetes Job.
- `ha.Client.FetchHistory` for reading `GET /api/history/period`, and
  `store.DeleteRange` / `store.RefreshContinuousAggregates` helpers.
- The container image now ships the `backfill` binary alongside the poller.

## [0.1.4] - 2026-06-12

### Security

- Bumped the Go toolchain and Docker builder image to 1.26.4 to pick up security fixes.

## [0.1.3] - 2026-05-12

### Changed

- GitHub release notes now include links to the published GHCR container image tags and an example `docker pull` command. Gitea releases remain changelog-only because the container image is GitHub-only.
- Bumped GitHub Actions to the latest major versions (Renovate `renovate/major-github-actions`): `docker/setup-qemu-action` v3 → v4, `docker/setup-buildx-action` v3 → v4, `docker/login-action` v3 → v4, `docker/metadata-action` v5 → v6, `docker/build-push-action` v6 → v7, and `actions/upload-artifact` v4 → v7.

## [0.1.2] - 2026-05-12

### Fixed

- Disabled the redundant internal checkout in `golang/govulncheck-action` so tag-triggered release workflows can run on GitHub without duplicate authorization headers. Gitea compatibility is unchanged.

## [0.1.1] - 2026-05-12

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

- Extended `.github/workflows/test.yml`: added `gofmt -l` formatting check, `govulncheck` against the Go vuln DB, a `docker build` step that verifies the bundled Dockerfile, and coverage profile upload as a workflow artifact. The workflow is now also `workflow_call`-able so the release workflow can gate on it.
- `.github/workflows/release.yml` now runs the test workflow as a prerequisite job (`needs: test`) before publishing, so a green release implies a green test run on the tagged code.
- The release workflow now builds and pushes a multi-arch container image (`linux/amd64`, `linux/arm64`) to `ghcr.io/<owner>/<repo>` on every `v*.*.*` tag. Image tags include the full `MAJOR.MINOR.PATCH`, `MAJOR.MINOR`, and `latest`. The docker-publish steps are guarded by `github.server_url == 'https://github.com'` so tag pushes from a Gitea mirror still produce a Gitea release; they simply skip the `ghcr.io` push.
- The `Dockerfile` now honors `$TARGETOS` / `$TARGETARCH` from BuildKit and runs the builder stage on `$BUILDPLATFORM`, so multi-arch builds cross-compile correctly instead of hard-coding `GOARCH=amd64`.
- `renovate.json` now explicitly enumerates `gomod`, `dockerfile`, `docker-compose`, and `github-actions` as enabled managers, with GitHub Actions bumps grouped into a single PR.

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
