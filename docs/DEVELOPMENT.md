# Development

This document covers building, running, and testing `hass-poller` locally.

## Prerequisites

- Go 1.22+ (the toolchain version is pinned in `go.mod`)
- Docker + Docker Compose (for the bundled TimescaleDB instance)
- A Home Assistant instance with a [long-lived access token](https://www.home-assistant.io/docs/authentication/#your-account-profile)

## Project layout

```
cmd/ha-timescale-poller/main.go   # entrypoint
internal/config/                   # environment + YAML config loading
internal/engine/                   # poll loop, change detection, scheduling
internal/filter/                   # allowlist/blocklist glob matching
internal/ha/                       # Home Assistant API client
internal/httpserver/               # /healthz + /metrics server
internal/store/                    # pgxpool, CopyFrom inserts, schema migration
schema.sql                         # TimescaleDB schema (embedded via go:embed)
schema.go                          # go:embed directive
Dockerfile                         # multi-stage build (distroless runtime)
docker-compose.yml                 # poller + TimescaleDB
docs/                              # architecture, development, diagrams
```

## Building

Binary:

```bash
go build -o ha-timescale-poller ./cmd/ha-timescale-poller
```

Docker image:

```bash
docker build -t ha-timescale-poller .
```

## Running locally without Docker

Point the binary at an existing TimescaleDB instance:

```bash
export HA_BASE_URL=https://homeassistant.local:8123
export HA_TOKEN=your-token
export PG_DSN=postgres://user:pass@localhost:5432/warehouse?sslmode=disable

./ha-timescale-poller
```

The schema (`schema.sql`) is embedded in the binary and applied automatically on startup.

## Running via systemd

```ini
[Unit]
Description=Home Assistant TimescaleDB Poller
After=network.target

[Service]
ExecStart=/usr/local/bin/ha-timescale-poller
Restart=always
EnvironmentFile=/etc/ha-timescale-poller.env

[Install]
WantedBy=multi-user.target
```

## Testing

```bash
go test ./...
```

Tests are pure-Go and do not require a running database — the store package uses a fake pool for unit coverage. End-to-end verification against a real TimescaleDB happens via `docker compose up` and the `/healthz` and `/metrics` endpoints.

## Editing the architecture diagram

The architecture diagram source is [`docs/diagrams/architecture.d2`](diagrams/architecture.d2). To regenerate the rendered SVG:

```bash
d2 docs/diagrams/architecture.d2 docs/diagrams/d2.svg
```

Install [d2](https://d2lang.com/) via your package manager.

## Editing the embedded schema

`schema.sql` is embedded into the binary at build time via `go:embed` (see `schema.go`). It is applied with `IF NOT EXISTS` / `if_not_exists` clauses, so adding policies and views is safe across restarts. Changes that require destructive migrations (renaming columns, changing types) need a separate migration step.
