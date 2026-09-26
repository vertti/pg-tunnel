# Development

## Building from source

```sh
git clone https://github.com/vertti/pg-tunnel.git
cd pg-tunnel
mise trust
mise install
mise run build   # creates bin/pg-tunnel
```

## Tasks

```sh
mise run fmt     # gofumpt and goimports
mise run lint    # Go, shell, workflows and configuration
mise run test    # Testify, race detection and coverage
mise run vuln    # reachable vulnerabilities; requires network
mise run ci      # all required checks plus a build
mise run package # local release archives in dist/; does not publish
```

Tests use `stretchr/testify` with all `testifylint` checks enabled. Install
PostgreSQL to run the local SCRAM and pgx integration tests; it is required in CI.
Coverage must meet the floor in `mise.toml`.

For client changes, update the [compatibility list](compatibility.md) with tested
versions and setup. Verify TLS hostname checking, a fresh login after credential
renewal, independent simultaneous sessions, and cleanup. Keep detailed test
results in the PR, and working notes in gitignored `PLAN.md`; public docs should
explain usage and current support rather than recount individual runs.

## Architecture

`internal/session` owns lifecycle and cleanup; `internal/awsdb` handles AWS;
`internal/libpq` manages credentials and TLS verification; `internal/process`
supervises clients. `internal/ssmplugin` runs AWS's official SSM code in an isolated
child process so upstream signal handlers cannot bypass our cleanup.

When updating the embedded plugin, keep its source revision and dependencies
pinned, including `twinj/uuid`. Run the WebSocket lifecycle tests and a live tunnel
check, and preserve the [upstream notices](../third_party/session-manager-plugin/).

## Live checks

These opt-in checks need an existing read-only AWS connection. Use
`aws-vault exec --server` if that is your usual credential source.

```sh
mise run build
# The same notebook kernel must reconnect after an idle network interruption:
mise x uv@0.12.10 -- uv run scripts/check-jupyter-recovery.py \
  --config /path/to/pg-tunnel.json --outage 30 reader
# A longer interruption must stop the kernel and clean up:
mise x uv@0.12.10 -- uv run scripts/check-jupyter-recovery.py \
  --config /path/to/pg-tunnel.json --outage 75 --expect-stop reader
# A query timeout during recovery must return; a fresh connection must work:
mise x uv@0.12.10 -- uv run scripts/check-jupyter-recovery.py \
  --config /path/to/pg-tunnel.json --outage 30 --active-query reader
# Suspend only the test process tree, then require recovery (not real OS sleep):
mise x uv@0.12.10 -- uv run scripts/check-jupyter-recovery.py \
  --config /path/to/pg-tunnel.json --suspend 120 reader
```

The script starts a disposable Jupyter server and interrupts only its SSM
connection. Both modes verify local credential and listener cleanup; failures
exit nonzero. They do not change system network settings or existing notebooks.

For container renewal, use the [Docker recipe](containers.md) with AWS credentials
valid for at least 20 minutes. Add a mount for
`scripts/check-container-renewal.sh` at `/run/check.sh`, then replace the entrypoint
and command with:

```sh
--entrypoint bash pg-tunnel-client \
  /run/check.sh /run/pg-tunnel.json development EXPECTED_DB_USER EXPECTED_DATABASE
```

The check waits 15m31s, requires a fresh TLS login with a changed password file,
and verifies local cleanup before the container exits. It does not test renewal
of the underlying AWS credentials.

## Releases

Release archives contain one binary plus licenses and notices. Builds disable
CGO and strip symbols; Linux binaries use UPX and all archives use `tar.xz`.
CI enforces a 25 MiB binary limit and tests packages on Linux amd64/arm64 and macOS arm64.
`pg-tunnel --version` reports the version and source commit.

From a clean main checkout, tag the chosen version:

```sh
git switch main
git pull --ff-only
git tag VERSION
git push origin VERSION
```

Use a `v`-prefixed version such as `v0.3.0`. The tag workflow runs checks and creates
a **draft** release; review the assets and notes before publishing. Manual workflow
runs build artifacts without publishing.

Publishing updates [the Homebrew tap](https://github.com/vertti/homebrew-tap).
The `release` environment needs `HOMEBREW_TAP_TOKEN` with contents-write access to
the tap. Dependabot maintains Go and GitHub Actions dependencies; mise tool pins
are updated separately.
