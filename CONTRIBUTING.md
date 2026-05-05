# Contributing

Thanks for your interest in `hass-poller`. This document covers the workflow conventions for the project.

## Reporting issues

Open an issue on the repository describing the problem, the version (or commit SHA) you're running, and steps to reproduce. For runtime issues, include relevant log lines and your `docker compose` configuration with secrets redacted.

## Development setup

See [`docs/DEVELOPMENT.md`](docs/DEVELOPMENT.md) for how to build, run, and test locally.

## Branch naming

All branches follow the form:

```
<type>/<kebab-case-description>
```

`<type>` must be one of:

| Type | When to use |
|---|---|
| `feat` | A new user-visible feature or capability |
| `fix` | A bug fix |
| `chore` | Maintenance work (deps, build, tooling, releases) |
| `docs` | Documentation only |
| `refactor` | Code change that neither fixes a bug nor adds a feature |
| `test` | Adding or fixing tests |

Examples: `feat/per-entity-retention`, `fix/null-unit-panic`, `docs/grafana-cookbook`, `chore/release-0.2.0`.

The `main` branch is the integration branch. Long-lived feature branches should be rebased onto `main` regularly.

## Commit messages

Use imperative mood in the subject line ("Add daily aggregate", not "Added daily aggregate" or "Adds daily aggregate"). Wrap the body at ~72 columns and explain *why* the change is being made when it is not obvious from the diff.

Conventional Commits prefixes (`feat:`, `fix:`, etc.) are encouraged in subject lines but not strictly required.

## Pull requests

1. Branch from the latest `main`.
2. Make your changes in small, logically-coherent commits.
3. Update [`CHANGELOG.md`](CHANGELOG.md) under the `[Unreleased]` heading. Group entries under `Added`, `Changed`, `Deprecated`, `Removed`, `Fixed`, or `Security`.
4. Update affected documentation (`README.md`, `docs/`) in the same PR as the behavior change.
5. Run `go test ./...` and ensure it passes.
6. Open a PR against `main`. Include a Summary section, a Context section if there is non-obvious motivation, and a Test plan.

## Releases

Releases follow [Semantic Versioning](https://semver.org/). The release process:

1. Open a `chore/release-<version>` PR that:
   - Renames the `[Unreleased]` heading in `CHANGELOG.md` to `[<version>] - YYYY-MM-DD`.
   - Adds a fresh empty `[Unreleased]` heading above it.
2. Merge the PR.
3. Tag the merge commit `v<version>` and push the tag.
4. Create a release on the Gitea repository linking to the changelog entry.
