# Development

```sh
mise run build  # stripped binary: bin/pg-tunnel
mise run test   # Testify, race detector, shuffled order, coverage.out
mise run fmt    # gofumpt and goimports
mise run lint   # strict analysis, formatting, workflows, module consistency
mise run vuln   # reachable dependency vulnerabilities; requires network
mise run ci     # the same complete checks as GitHub Actions
```

Tests use `stretchr/testify`, with all `testifylint` checks enabled. Suppressions
must name the rule and explain their scope. CI runs one Linux job on pull
requests and pushes to `main`.

The core interfaces live in `internal/session`. AWS discovery/authentication and
SSM transport live in `internal/awsdb`; `internal/libpq` owns credential files and
the TLS/IAM readiness check; `internal/process` owns process groups. The small
`internal/ssmplugin` adapter runs AWS code in an isolated copy of our executable.
Profiles and CLI wiring remain separate from those providers.

`pg-tunnel --version` reports Go's embedded module version and, for builds from
Git, the full commit. Tagged builds use the release version; other commits use
Go's timestamped version, and modified checkouts include `+dirty`. Builds without
version metadata report `dev`. No custom linker flags are needed. Release builds
must use a clean checkout with tags available.

See [the footprint measurements](footprint.md) for the initial size budget.

The official SSM source is pinned to commit
[`930a08e65d3a`](https://github.com/aws/session-manager-plugin/commit/930a08e65d3a378eeeebb7f1bcf67eae7d860ae0).
Its port handler registers in an internal `__ssm` child mode, before our usual
signal setup. AWS's signal handlers and `os.Exit` calls therefore cannot bypass
the supervisor's credential cleanup. Session tokens go through the child
environment, not command arguments. We maintain the adapter, not the SSM protocol.

Upstream is an executable-oriented project without a root `go.mod`; keep its
revision and dependencies pinned, including the historical `twinj/uuid` version.
Run the local WebSocket cancellation test and live remote-host forwarding checks
when upgrading it. Upstream's built-in version is `1.3.0.0` for feature negotiation;
the pinned commit identifies the actual source revision. Preserve the
[upstream license and notices](../third_party/session-manager-plugin/) in release
packages alongside the binary.
