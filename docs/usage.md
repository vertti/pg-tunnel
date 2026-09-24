# Configuration and usage

Both `run` and `connect` select one configuration file, in this order:

1. The explicit `--config PATH`, if supplied before the profile name.
2. `pg-tunnel.json` in the current directory.
3. The shared user configuration:
   - macOS: `~/Library/Application Support/pg-tunnel/pg-tunnel.json`
   - Linux: `$XDG_CONFIG_HOME/pg-tunnel/pg-tunnel.json`, or
     `~/.config/pg-tunnel/pg-tunnel.json` when `XDG_CONFIG_HOME` is unset or empty.

Use the user configuration to share profiles across worktrees without repeating
`--config`. Files are not merged: an invalid or unreadable selected file, or a
missing profile within it, is an error. An explicit missing file never falls back
to another location. Certificate paths are relative to the selected file, so move
its CA bundle too if the profile uses a relative `sslrootcert` path.

## Interactive setup

AWS Vault is optional. Use your normal AWS credentials or select a named AWS
profile directly (`--profile` and `--aws-profile` are aliases):

```sh
pg-tunnel init --region eu-central-1
# Or use a named AWS profile (region comes from it when configured):
pg-tunnel init --profile dev
```

For an SSO profile, log in first with `aws sso login --profile dev`. The selected
AWS profile is saved in the connection profile, so subsequent `run` and `connect`
commands reuse it. Profiles whose credentials exist only in AWS Vault's keychain
still need AWS Vault to supply them. Add `--config pg-tunnel.json` to save locally.

The wizard shows the account, lists RDS PostgreSQL instances in that region, and
suggests running EC2 hosts that are online in SSM. Hosts in the database's VPC
appear first. Choose IAM or Secrets Manager authentication, enter an existing
database user and database name, then review the profile. After you confirm, the
wizard opens a temporary tunnel, verifies TLS and database authentication, and cleans up the session before saving. It sends no
SQL queries. A failed test, cancellation, or cleanup error leaves the configuration
unchanged. RDS CA certificates are managed automatically. Profiles are added to
the shared user config by default; an existing name is never replaced.
Use `--config` for the project file if one would shadow your shared configuration.

Discovery needs `rds:DescribeDBInstances`, `ec2:DescribeInstances`, and
`ssm:DescribeInstanceInformation`. It also calls STS `GetCallerIdentity` to show
the account. If jump-host discovery is unavailable, you can enter the SSM target
manually. Verification also needs the connection permissions listed below.
Secret values are read only during the confirmed connection test. For password
authentication, you can select the RDS-linked master-user secret or enter another
secret name/ARN. The master username is suggested only for its linked secret;
other secrets require an explicit database user.

| Setting | Meaning |
| --- | --- |
| `db_instance` | Discover an RDS PostgreSQL instance's endpoint and port. |
| `host` | Alternative explicit database endpoint; mutually exclusive with `db_instance`. |
| `port` | Remote port for an explicit host, default `5432`. |
| `database`, `user` | PostgreSQL database and existing database user. |
| `auth` | `iam` (default) or `secrets-manager`; never falls back between modes. |
| `secret_id` | Secret name or full ARN; required only for `auth: "secrets-manager"`. |
| `target` | Explicit SSM managed instance ID. |
| `jump_tag` | Alternative EC2 `Name` tag; must match exactly one running instance. |
| `region`, `aws_profile` | Optional overrides for the standard AWS SDK configuration. |
| `local_port` | Optional local port; default `0` selects an available port. |
| `sslrootcert` | Optional custom PEM trust bundle for RDS instances; required for an explicit `host`. |

When `sslrootcert` is omitted, `init`, `run`, and `connect` download the official
[AWS RDS CA bundle](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.SSL.html)
over verified HTTPS and cache it under the OS user cache directory in
`pg-tunnel/certificates`. Commercial, GovCloud, and China bundles are kept separate.
The bundle is checked for updates after 30 days when a command starts. A failed
refresh keeps a usable cached bundle and prints a warning; a missing, invalid, or
expired bundle must be downloaded successfully. Database connections always use
`verify-full`, including hostname and certificate-expiry checks.

Use `init --sslrootcert /path/to/ca.pem` or set `sslrootcert` in a profile to manage
trust yourself. Custom files are never refreshed or replaced. Remove an existing
RDS profile's `sslrootcert` setting to opt into automatic management.

The SSM node must reach the database and support
`AWS-StartPortForwardingSessionToRemoteHost`. Your identity needs
`ssm:StartSession` and `ssm:TerminateSession`. Discovery also needs
`rds:DescribeDBInstances` and, when using a jump tag,
`ec2:DescribeInstances`. IAM authentication additionally needs `rds-db:connect`,
IAM enabled on the instance, and `rds_iam` granted to the database user. Password
authentication needs `secretsmanager:GetSecretValue` for the selected secret and
`kms:Decrypt` when it uses a customer-managed KMS key. The user's SQL permissions
are defined in PostgreSQL; the utility does not grant read or write access.

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

## Secrets Manager passwords

Choose **Secrets Manager password** in `init`, or add these fields to a profile:

```json
{
  "auth": "secrets-manager",
  "secret_id": "application/reader"
}
```

The secret must contain a JSON `SecretString` with nonempty `username` and
`password` fields. The username must match the profile's `user`; optional `host`
and `port` must match the resolved database, and optional `engine` must be
`postgres`. The profile's `database` selects the database. Binary secrets and
alternating-user rotation are not supported. The configuration stores only the
secret identifier; passwords are never copied into it.

A user with `rds_iam` membership must use IAM authentication: on RDS PostgreSQL,
[IAM takes precedence over password authentication](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.html).

## Client behavior

`run` verifies TLS and database authentication before starting the command.
It supplies `PGSERVICE`, `PGSERVICEFILE`, and `PGPASSFILE` pointing to private
per-session files, replacing inherited `PG*` settings. Command output is left on
stdout; tunnel diagnostics go to stderr. The real database hostname remains the
TLS identity, while `hostaddr=127.0.0.1` routes libpq through the local tunnel.

A notebook launched through the utility can use a libpq-based driver directly:

```python
import psycopg

conn = psycopg.connect("")
```

Notebooks in the same local JupyterLab server share one tunnel. Launching each
Lab server through a separate `pg-tunnel run`, for example one per worktree,
creates separate tunnels and private credential files. Leave `local_port` unset
or `0` to choose available ports automatically; reusing a fixed port causes a
conflict. Closing or restarting a notebook leaves its server's tunnel running;
shutting down the Lab server closes it.

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

IAM tokens are refreshed three minutes before their reported expiry. Secrets
Manager passwords are reread from `AWSCURRENT` every five minutes. Changed
credentials must pass a fresh TLS/database login before replacing the password
file. Passwords have no reported expiry; new connections may fail between database
password rotation and the next successful refresh.

Transient failures retry after five seconds, with bounded exponential backoff up
to one minute. A failed replacement leaves the last password file intact. Token expiry
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
sessions. AWS session limits provide an additional backstop. Unlike IAM tokens,
passwords left after a crash do not expire automatically; run `pg-tunnel cleanup`
to remove abandoned files. SSH/VPN transports, Windows, and additional client
adapters remain later work.
