# pg-tunnel

A planned developer utility for connecting PostgreSQL clients to private databases,
starting with AWS RDS and Systems Manager Session Manager.

The intended workflow manages database discovery, tunneling, credentials, IAM
token refresh, client configuration, and cleanup for a command or interactive
session.

Status: initial CLI scaffold. Database tunneling is not implemented yet.

## Development setup

Install [mise](https://mise.jdx.dev/getting-started.html), then run:

```sh
mise trust
mise install
mise exec -- go version
```

Development tools are managed through `mise.toml`, with exact versions pinned
for reproducibility. Run Go commands through `mise exec -- go ...`, or activate
mise in your shell. Add future development tools to the same configuration.

## Build and quality checks

```sh
mise run build  # bin/pg-tunnel
mise run test   # Testify tests, race detector, shuffled order, coverage.out
mise run fmt    # apply gofumpt formatting and organize imports
mise run lint  # strict static analysis, formatting, workflows, module consistency
mise run vuln  # reachable dependency vulnerabilities (requires network access)
mise run ci    # all checks, tests, and the build
```

The initial CLI supports `--help` and `--version`; tunneling is not implemented.
Tests use `stretchr/testify`: `require` for prerequisites and `assert` for
independent expectations. Prefer behavioral and failure-path tests, including
cleanup, cancellation, and credential renewal as those features are added.

Golangci-lint checks production and test code, including security, resource
handling, context propagation, error handling, complexity, and Testify usage.
Formatting is enforced during linting. Linter suppressions must name the rule
and explain why it does not apply. GitHub Actions runs `mise run ci` in one Linux
job on pull requests and pushes to `main`.
