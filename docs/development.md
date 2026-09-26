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
mise run build  # stripped binary: bin/pg-tunnel
mise run test   # Testify, race detector, shuffled order, coverage.out
mise run fmt    # gofumpt and goimports
mise run lint   # strict analysis, formatting, workflows, module consistency
mise run vuln   # reachable dependency vulnerabilities; requires network
mise run package # four local archives + checksums; never publishes
mise run ci     # the same complete checks as GitHub Actions
```

Tests use `stretchr/testify`, with all `testifylint` checks enabled. Suppressions
must name the rule and explain their scope. `mise run test` fails when total
statement coverage drops below the floor in `mise.toml`; raise it as coverage grows. The main CI job runs on Linux. A separate
release workflow builds all four archives on pull requests and smoke-tests them
on native macOS/Linux amd64/arm64 runners.

The SCRAM and pgx client integration tests start isolated local PostgreSQL
clusters and remove them afterward. Install PostgreSQL locally to run them;
`mise run test` finds binaries through `pg_config` when needed. They are skipped locally if PostgreSQL is absent,
and required in CI (which uses the runner's installed PostgreSQL).

Client integration changes must update the [compatibility list](compatibility.md)
with versions, setup, and completed verification. Include a smallest concrete
test with an observable pass/fail result; keep planned checks separate from evidence.
Coverage includes cross-package calls so integration tests count toward the code
they exercise, including the copyable client examples.

The opt-in [Jupyter interruption check](recovery.md#repeat-the-check) uses an
existing read-only AWS connection. It is separate from CI and never changes
system network settings. The opt-in [container renewal check](containers.md#repeat-the-renewal-and-cleanup-check)
verifies a fresh IAM login and local cleanup inside Docker; ShellCheck runs in CI,
while the AWS check is manual.

The core interfaces live in `internal/session`. AWS discovery/authentication and
SSM transport live in `internal/awsdb`; `internal/libpq` owns credential files and
the TLS/database readiness check; `internal/process` owns process groups. The small
`internal/ssmplugin` adapter runs AWS code in an isolated copy of our executable.
Connection configuration (`internal/profile`) and CLI wiring remain separate from
those providers.

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
`mise run package` to build local `tar.xz` snapshots in `dist/`. On Linux,
GoReleaser also packs Linux binaries with UPX `-9` (pinned through mise).
macOS snapshots skip UPX when it is unavailable; official releases are packaged
on Linux, and CI requires both Linux binaries to pass `upx -t`. Archives contain the
binary, project license, runtime dependency licenses/notices, Go's license, and
AWS's upstream notices. `checksums.txt` uses SHA-256. License discovery inspects
the command's imports, not just direct entries in `go.mod`.

Builds disable CGO, strip symbols and local paths, and use the Git commit time
for archive timestamps. Use the same Go/tool versions, source commit and tags
when comparing checksums. The release checks enforce the 25 MiB binary budget.
Platform smoke tests verify checksums, notices, version/commit, CLI startup,
and SSM startup cancellation and graceful shutdown using the packaged executable
against a local WebSocket fixture; they do not log into AWS. Signing/notarization
is not yet configured.

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

Publishing the release triggers the Homebrew workflow, which renders
`Formula/pg-tunnel.rb` from the release's `checksums.txt` and pushes it to
[vertti/homebrew-tap](https://github.com/vertti/homebrew-tap). It runs in the
`release` environment, which only `v*` tags can deploy to, and needs that
environment's `HOMEBREW_TAP_TOKEN` secret with contents write access to the tap.

Dependabot checks Go modules and GitHub Actions weekly, grouping minor/patch
updates per ecosystem. Security alerts and security-fix PRs are enabled on GitHub.
Mise tool pins are maintained separately.
