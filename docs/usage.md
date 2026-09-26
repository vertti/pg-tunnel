# Configuration and usage

A connection (the `CONNECTION` argument of `run` and `connect`) is a named entry
under `profiles` in `pg-tunnel.json`. It is unrelated to AWS profiles, which
select AWS credentials.

Both `run` and `connect` select one configuration file, in this order:

1. The explicit `--config PATH`, if supplied before the connection name.
2. `pg-tunnel.json` in the current directory.
3. The shared user configuration:
   - macOS: `~/Library/Application Support/pg-tunnel/pg-tunnel.json`
   - Linux: `$XDG_CONFIG_HOME/pg-tunnel/pg-tunnel.json`, or
     `~/.config/pg-tunnel/pg-tunnel.json` when `XDG_CONFIG_HOME` is unset or empty.

Use the user configuration to share connections across worktrees without repeating
`--config`. Files are not merged: an invalid or unreadable selected file, or a
missing connection within it, is an error. An explicit missing file never falls back
to another location. Certificate paths are relative to the selected file, so move
its CA bundle too if the connection uses a relative `sslrootcert` path.

A `pg-tunnel.json` picked up from the current directory may not set `host` or
`sslrootcert`. Otherwise a cloned repository could supply a CA it controls and let
a jump host impersonate the database to capture your IAM token or database
password. Select such a file with `--config` to trust it.

## Interactive setup

AWS Vault is optional. Use your normal AWS credentials or select a named AWS
profile directly (`--profile` is an alias of `--aws-profile`):

```sh
pg-tunnel init --region eu-central-1
# Or use a named AWS profile (region comes from it when configured):
pg-tunnel init --aws-profile dev
```

For an SSO profile, log in first with `aws sso login --profile dev`. The selected
AWS profile is saved in the connection, so subsequent `run` and `connect`
commands reuse it. AWS profiles whose credentials exist only in AWS Vault's keychain
still need AWS Vault to supply them. Add `--config pg-tunnel.json` to save locally.

The wizard shows the account, lists RDS PostgreSQL instances in that region, and
suggests running EC2 hosts that are online in SSM. Hosts in the database's VPC
appear first. Choose IAM or Secrets Manager authentication, enter an existing
database user and database name, then review the connection. After you confirm, the
wizard opens a temporary tunnel, verifies TLS and database authentication, and cleans up the session before saving. It also reads PostgreSQL role metadata to warn about privileged access. A failed test, cancellation, or cleanup error leaves the configuration
unchanged. RDS CA certificates are managed automatically. Connections are added to
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
| `db_cluster` | Discover an Aurora PostgreSQL cluster endpoint and port. |
| `cluster_endpoint` | `writer` (default) or `reader`; requires `db_cluster`. |
| `host` | Explicit database endpoint. Set exactly one of `db_instance`, `db_cluster`, or `host`. |
| `port` | Remote port for an explicit host, default `5432`. |
| `database`, `user` | PostgreSQL database and existing database user. |
| `environment` | Optional `development`, `staging`, or `production`; production prints a startup warning. |
| `auth` | `iam` (default) or `secrets-manager`; never falls back between modes. |
| `secret_id` | Secret name or full ARN; required only for `auth: "secrets-manager"`. |
| `target` | Explicit SSM managed instance ID. |
| `jump_tag` | Alternative EC2 `Name` tag; must match exactly one running instance. |
| `region`, `aws_profile` | Optional overrides for the standard AWS SDK configuration. |
| `local_port` | Optional local port; default `0` selects an available port. |
| `sslrootcert` | Optional custom PEM trust bundle for RDS instances or Aurora clusters; required for an explicit `host`. |

When `sslrootcert` is omitted, `init`, `run`, and `connect` download the official
[AWS RDS CA bundle](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.SSL.html)
over verified HTTPS and cache it under the OS user cache directory in
`pg-tunnel/certificates`. Commercial, GovCloud, and China bundles are kept separate.
The bundle is checked for updates after 30 days when a command starts. A failed
refresh keeps a usable cached bundle and prints a warning; a missing, invalid, or
expired bundle must be downloaded successfully. Database connections always use
`verify-full`, including hostname and certificate-expiry checks.

Use `init --sslrootcert /path/to/ca.pem` or set `sslrootcert` in a connection to manage
trust yourself. Custom files are never refreshed or replaced. Remove an existing
RDS connection's `sslrootcert` setting to opt into automatic management.

The SSM node must reach the database and support
`AWS-StartPortForwardingSessionToRemoteHost`. Your identity needs
`ssm:StartSession` and `ssm:TerminateSession`; resuming an interrupted connection
also needs `ssm:ResumeSession`. When `TerminateSession` fails,
`ssm:DescribeSessions` confirms the session already ended; without it, that
cleanup reports an error. Discovery also needs `rds:DescribeDBInstances` for
`db_instance` or `rds:DescribeDBClusters` for `db_cluster`, and
`ec2:DescribeInstances` when using a jump tag. IAM authentication additionally
needs `rds-db:connect`, IAM enabled on the instance or cluster, and `rds_iam`
granted to the database user. Password authentication needs
`secretsmanager:GetSecretValue` for the selected secret and `kms:Decrypt` when it
uses a customer-managed KMS key. The user's SQL permissions are defined in
PostgreSQL; the utility does not grant read or write access.

### Aurora cluster endpoints

Configure a cluster directly in your `pg-tunnel.json`:

```json
{
  "profiles": {
    "analytics": {
      "db_cluster": "my-cluster",
      "cluster_endpoint": "reader",
      "database": "app",
      "user": "app_reader",
      "target": "i-your-ssm-host",
      "region": "eu-central-1"
    }
  }
}
```

Use `writer` (or omit `cluster_endpoint`) for the cluster's writer endpoint.
Each new `run` or `connect` looks up the selected AWS endpoint; that hostname is
used for SSM forwarding, IAM signing and TLS verification. RDS CA management
works automatically. `init` currently lists individual instances; cluster
selection is configured by editing the file.

The [writer endpoint](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/Aurora.Endpoints.Cluster.html)
routes to the primary. The
[reader endpoint](https://docs.aws.amazon.com/AmazonRDS/latest/AuroraUserGuide/Aurora.Endpoints.Reader.html)
can also reach the writable primary when there are no replicas. Reader selection
is not an access-control guarantee; use a database role with suitable permissions.
pg-tunnel does not choose a replica itself or preserve transactions through failover.

For Secrets Manager, any `host` in the secret must match the selected endpoint.
A secret naming the writer endpoint is rejected for a reader connection. The tool
does not relax that check or automatically select a cluster's master secret.
See [verification status](compatibility.md#database-endpoints) before relying on
Aurora behavior that has not been tested live.

### AWS credentials

For AWS Vault, omit `aws_profile` from the connection and use a renewable
credential source:

```sh
aws-vault exec --server YOUR_PROFILE -- pg-tunnel run development -- psql
```

Static temporary credentials in environment variables cannot refresh themselves.
The utility reports AWS credential expiry when available and recognizes
`AWS_CREDENTIAL_EXPIRATION` for environment credentials. SSO sessions can also
require a fresh login after their underlying login session expires.

## Secrets Manager passwords

Choose **Secrets Manager password** in `init`, or add these fields to a connection:

```json
{
  "auth": "secrets-manager",
  "secret_id": "application/reader"
}
```

The secret must contain a JSON `SecretString` with nonempty `username` and
`password` fields. The username must match the connection's `user`; optional `host`
and `port` must match the resolved database, and optional `engine` must be
`postgres`. The connection's `database` selects the database. Binary secrets and
alternating-user rotation are not supported. The configuration stores only the
secret identifier; passwords are never copied into it.

A user with `rds_iam` membership must use IAM authentication: on RDS PostgreSQL,
[IAM takes precedence over password authentication](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.html).

## Connection warnings

`init` asks for the environment; choose **Production** to print a prominent
warning whenever the connection starts. For existing connections, set
`"environment": "production"`. Unspecified environments are not classified.

At startup, a read-only PostgreSQL catalog query checks the logged-in user's
`SUPERUSER`, `CREATEROLE`, `CREATEDB`, and `BYPASSRLS` attributes and membership in
`rds_superuser`. Privileged access prints a warning before your command starts.
If catalog inspection is unavailable, pg-tunnel reports that privileges are
unknown and lets the verified connection proceed. These are reminders, not access
controls; absence of a warning does not mean a user cannot modify data.
Warnings go to stderr and do not prompt or repeat during credential refresh.

## Client behavior

`run` verifies TLS and database authentication before starting the command.
It supplies `PGSERVICE`, `PGSERVICEFILE`, and `PGPASSFILE` pointing to private
per-session files, replacing inherited `PG*` settings. Command output is left on
stdout; tunnel diagnostics go to stderr. The real database hostname remains the
TLS identity, while `hostaddr=127.0.0.1` routes libpq through the local tunnel.
See the [client compatibility list](compatibility.md) for verified setups and
limitations. Go pgx pools need the [connection hook](compatibility.md#go-pgx-pools)
to handle local routing and reread refreshed credentials.

The tunnel listens on `127.0.0.1`, so other accounts on the same machine can
reach the database through it while it runs. They still need database
credentials, which stay in files only your account can read.

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

This keeps the session alive and prints the three settings to stdout as shell
`export` lines; paste them into another terminal or configure the client's
service and password file paths. Diagnostics go to stderr. Ctrl-C closes the
session and exits 0. GUI support depends on the client's libpq/service-file
capabilities.

See [network interruption behavior and its live check](recovery.md) for recovery
limits when a notebook server loses its SSM connection.

For Docker or devcontainers, run the client and pg-tunnel together inside the
container; see the [container recipe and renewal check](containers.md).

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
credentials, and attempts both local and remote tunnel cleanup. `run` exits with
the command's status. pg-tunnel's own failures exit 1, usage mistakes exit 2, and
signals exit 128 plus the signal number. SIGINT, SIGTERM, and SIGHUP (closing the terminal) are forwarded;
processes that fail to exit are killed after a grace period. Ctrl-Z suspends the
command together with pg-tunnel, and `fg` resumes both.

After an uncatchable termination or machine crash, a new session automatically
removes abandoned credential directories. Active sessions are protected by
process-held directory locks. Recovery can also be run explicitly:

```sh
pg-tunnel cleanup
```

SIGKILL cannot trigger immediate cleanup. The embedded SSM child notices that
pg-tunnel is gone and ends its remote session; the command's own descendants may
need separate termination. Unlike IAM tokens,
passwords left after a crash do not expire automatically; run `pg-tunnel cleanup`
to remove abandoned files. SSH/VPN transports, Windows, and additional client
adapters remain later work.
