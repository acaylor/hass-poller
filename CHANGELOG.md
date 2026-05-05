# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Added an MIT `LICENSE` file for the planned public release.
- Added this `CHANGELOG.md` to track notable changes going forward.
- Added a daily continuous aggregate `ha_numeric_1d` (`avg`/`min`/`max`/`count` per entity per day) with no retention policy, so long-range historical data is retained forever at daily resolution.
- Added a 1-year retention policy on the hourly continuous aggregate `ha_numeric_1h` so it does not grow unboundedly.
- Added a `Backups` section to the README documenting how to take and restore `pg_dump` snapshots, and added `backups/` to `.gitignore` so dump files are not committed.

### Changed

- Expanded the default `ENTITY_BLOCKLIST` in `docker-compose.yml` to drop redundant or low-signal sensors at ingestion time:
  - `*_power_minute_average` (sliding-window averages already derivable from energy counters)
  - `*_signal_level`, `*_disk_write_rate`, `*_storage_used` (device-health metrics not relevant to home energy/climate analytics)
  - Dead Emporia Vue3 phantom circuits (`vue3_energy_today_*`, `vue3_energy_this_month_*`, and the unsuffixed/`_2`-suffixed `range_`/`dryer_` circuits where the active circuit is `_3`).
- Updated the README's database schema and Grafana sections to describe the tiered raw → hourly → daily layout.
