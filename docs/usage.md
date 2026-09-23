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

After authenticating to AWS, run:

```sh
pg-tunnel init --region eu-central-1
# Or select an AWS profile and a project-local destination:
pg-tunnel init --aws-profile dev --region eu-central-1 --config pg-tunnel.json
```

The wizard shows the account, lists RDS PostgreSQL instances in that region, and
suggests running EC2 hosts that are online in SSM. Hosts in the database's VPC
appear first; network access and database login are verified when you connect.
Enter an existing IAM database user, database name, and trusted CA PEM path, then
review the profile before saving. It adds to the shared user config by default,
refuses to replace an existing profile name, and never changes AWS resources.
Use `--config` for the project file if one would shadow your shared configuration.

Discovery needs `rds:DescribeDBInstances`, `ec2:DescribeInstances`, and
`ssm:DescribeInstanceInformation`. It also calls STS `GetCallerIdentity` to show
the account. If jump-host discovery is unavailable, you can enter the SSM target
manually. RDS-linked master-user secret ARNs are shown as metadata only: the
wizard does not read secret values, infer database users from secret names, or
configure password authentication. This initial setup supports RDS PostgreSQL
with IAM authentication; CA bundle downloading and broader secret discovery
remain separate work.

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
