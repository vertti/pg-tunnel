# pg-tunnel

Run PostgreSQL clients against private databases with managed tunnels and IAM
credentials. The first backend supports AWS RDS PostgreSQL through an SSM jump
host on macOS and Linux.

```sh
pg-tunnel run development -- psql
pg-tunnel run development -- python analysis.py
pg-tunnel run development -- uv run jupyter lab
```

This is an early implementation. Automated tests exercise local TLS/database
handshakes, subprocesses, token renewal, and cleanup; a live AWS acceptance test
is still required before a release.

## Setup

Install [mise](https://mise.jdx.dev/getting-started.html), then:

```sh
mise trust
mise install
mise run build
cp pg-tunnel.example.json pg-tunnel.json
curl --fail --show-error --location \
  https://truststore.pki.rds.amazonaws.com/global/global-bundle.pem \
  --output global-bundle.pem
```

Edit `pg-tunnel.json` with your database, database user, jump host, AWS region,
and AWS profile. Both the local profile and downloaded CA bundle are gitignored.
The example contains no real infrastructure identifiers or credentials.

```sh
mise exec -- ./bin/pg-tunnel run development -- psql
```

Mise pins the Go toolchain, development checks, and official AWS Session Manager
plugin. The runtime requires the `pg-tunnel` binary and `session-manager-plugin`
on PATH. It does not require the AWS CLI, Go, or the linters. AWS CLI or AWS Vault
may still be useful for your organization's login workflow.

## Connection profiles

Profiles are loaded from `pg-tunnel.json` in the current directory. Use an
explicit file with `pg-tunnel run --config /path/to/profiles.json NAME -- COMMAND`.
Certificate paths are relative to the profile file.

| Setting | Meaning |
| --- | --- |
| `db_instance` | Discover an RDS PostgreSQL instance's endpoint and port. |
| `host` | Alternative explicit database endpoint; mutually exclusive with `db_instance`. |
| `port` | Remote port for an explicit host, default `5432`. |
| `database`, `user` | PostgreSQL database and IAM-enabled database user. |
| `target` | Explicit SSM managed instance ID. |
| `jump_tag` | Alternative EC2 `Name` tag; must match exactly one running instance. |
| `region`, `aws_profile` | Optional overrides for the standard AWS SDK configuration. |
| `local_port` | Optional local port; default `0` selects an available port. |
| `sslrootcert` | Required PEM trust bundle for full database certificate verification. |

The SSM node must reach the database and support
`AWS-StartPortForwardingSessionToRemoteHost`. Your identity needs
`ssm:StartSession`, `ssm:TerminateSession`, and `rds-db:connect`. Discovery also
needs `rds:DescribeDBInstances` and, when using a jump tag,
`ec2:DescribeInstances`. The database must enable IAM authentication and grant
`rds_iam` to the selected database user. The user's SQL permissions are defined
in PostgreSQL; the utility does not grant read or write access.

For AWS Vault, omit `aws_profile` from the connection profile and use a renewable
credential source:

```sh
aws-vault exec --server YOUR_PROFILE -- \
  mise exec -- ./bin/pg-tunnel run development -- psql
```

Static temporary credentials in environment variables cannot refresh themselves.
The utility reports AWS credential expiry when available and recognizes
`AWS_CREDENTIAL_EXPIRATION` for environment credentials. SSO sessions can also
require a fresh login after their underlying login session expires.

## Client behavior

`run` verifies TLS and IAM database authentication before starting the command.
It supplies `PGSERVICE`, `PGSERVICEFILE`, and `PGPASSFILE` pointing to private
per-session files, replacing inherited `PG*` settings. Command output is left on
stdout; tunnel diagnostics go to stderr. The real database hostname remains the
TLS identity, while `hostaddr=127.0.0.1` routes libpq through the local tunnel.

A notebook launched through the utility can use a libpq-based driver directly:

```python
import psycopg

conn = psycopg.connect("")
```

Already running notebook servers do not inherit these settings. Drivers that do
not use libpq, applications with explicit connection strings, and clients that
cache passwords may need their own integration. `DATABASE_URL` is not rewritten.

For a separately launched client:

```sh
pg-tunnel connect development
```

This keeps the session alive and prints the three environment settings. Supply
those values to the other client or configure its service/password file paths.
GUI support depends on the client's libpq/service-file capabilities.

## Renewal and cleanup

IAM tokens are refreshed three minutes before their reported expiry. Transient
failures retry after five seconds, with bounded exponential backoff up to one
minute. A failed replacement leaves the last password file intact. Token expiry
affects new logins, not established database connections. A running process's
password environment variable cannot be updated, so the utility uses file lookup.

Each session owns a mode-0700 directory and mode-0600 credential files. Password
updates use atomic replacement. The shared `~/.pgpass` is never modified.
Shutdown joins the refresh worker, stops the child process group, deletes private
credentials, and attempts both local and remote tunnel cleanup. Child exit codes
are preserved. SIGINT and SIGTERM are forwarded; processes that fail to exit are
killed after a grace period.

After an uncatchable termination or machine crash, a new session automatically
removes abandoned credential directories. Active sessions are protected by
process-held directory locks. Recovery can also be run explicitly:

```sh
pg-tunnel cleanup
```

SIGKILL cannot trigger immediate cleanup. Orphaned child processes or SSM sessions
may need separate termination; recovery removes credential files, not remote
sessions. AWS session limits provide an additional backstop. This first version
supports IAM only; Secrets Manager passwords, SSH/VPN transports, Windows, and
additional client adapters are later work.

## Development

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
the TLS/IAM readiness check; `internal/process` owns process groups. Profiles and
CLI wiring remain separate from those providers.

See [the footprint measurements](docs/footprint.md) for the initial size budget.
