# Development

```sh
mise run build  # stripped binary: bin/pg-tunnel
mise run test   # Testify, race detector, shuffled order, coverage.out
mise run fmt    # gofumpt and goimports
mise run lint   # strict analysis, formatting, workflows, module consistency
mise run vuln   # reachable dependency vulnerabilities; requires network
mise run package # four local archives + checksums; never publishes
mise run ci     # the same complete checks as GitHub Actions
```

Tests use `stretchr/testify`, with all `testifylint` checks enabled. Suppressions
must name the rule and explain their scope. The main CI job runs on Linux. A separate
release workflow builds all four archives on pull requests and smoke-tests them
on native macOS/Linux amd64/arm64 runners.

The SCRAM integration test starts an isolated local PostgreSQL cluster and removes
it afterward. Install PostgreSQL locally to run it; `mise run test` finds binaries
through `pg_config` when needed. It is skipped locally if PostgreSQL is absent,
and required in CI (which uses the runner's installed PostgreSQL).

The core interfaces live in `internal/session`. AWS discovery/authentication and
SSM transport live in `internal/awsdb`; `internal/libpq` owns credential files and
the TLS/database readiness check; `internal/process` owns process groups. The small
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

## Releases

Release tools are pinned in mise; they are not runtime dependencies. Run
`mise run package` to build local snapshots in `dist/`. Archives contain the
binary, project license, runtime dependency licenses/notices, Go's license, and
AWS's upstream notices. `checksums.txt` uses SHA-256. License discovery inspects
the command's imports, not just direct entries in `go.mod`.

Builds disable CGO, strip symbols and local paths, and use the Git commit time
for archive timestamps. Use the same Go/tool versions, source commit and tags
when comparing checksums. The release checks enforce the 25 MiB binary budget.
Platform smoke tests verify checksums, notices, version/commit, and CLI startup;
they do not log into AWS. Signing/notarization is not yet configured.

After merging a release commit to main and choosing a version:

```sh
git switch main
git pull --ff-only
git tag v0.1.0
git push origin v0.1.0
```

The tag workflow verifies the commit belongs to main, reruns quality checks,
builds packages and smoke-tests each platform, then creates a **draft** GitHub
release. Review its assets and notes before publishing. Failed checks produce
no draft. A manual workflow run on a branch produces CI artifacts only.

Dependabot checks Go modules and GitHub Actions weekly, grouping minor/patch
updates per ecosystem. Security alerts and security-fix PRs are enabled on GitHub.
Mise tool pins are maintained separately.
