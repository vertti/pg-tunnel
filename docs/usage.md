# Configuration and usage

A connection (the `CONNECTION` argument of `run` and `connect`) is a named entry
under `profiles` in `pg-tunnel.json`. It is unrelated to AWS profiles, which
select AWS credentials.

Both `run` and `connect` select one configuration file, in this order:

1. The explicit `--config PATH`, supplied before the connection name.
2. `pg-tunnel.json` in the current directory.
3. The shared user configuration:
   - macOS: `~/Library/Application Support/pg-tunnel/pg-tunnel.json`
   - Linux: `$XDG_CONFIG_HOME/pg-tunnel/pg-tunnel.json`, or
     `~/.config/pg-tunnel/pg-tunnel.json` when `XDG_CONFIG_HOME` is unset.

Files are not merged. Use the shared user config to reuse connections across
worktrees; a project config takes precedence. Relative certificate paths are
resolved from the selected config file.

Project configs containing `host` or `sslrootcert` require an explicit `--config`
to trust them, preventing an untrusted checkout from substituting a database or CA.

## Interactive setup

```sh
pg-tunnel init --region eu-central-1
# Or select an AWS profile; --profile is an alias:
pg-tunnel init --aws-profile dev
```

For SSO, first run `aws sso login --profile dev`. AWS Vault is optional; pg-tunnel
uses the standard AWS credential sources and saves your selected AWS profile.

The wizard finds RDS databases and SSM jump hosts, then asks for your database
name, user and authentication method. It tests TLS and database access before
saving. Add `--config pg-tunnel.json` to save in the project instead of your user
config. Existing connection names are not overwritten.

| Setting | Meaning |
| --- | --- |
| `db_instance` | Discover an RDS PostgreSQL instance's endpoint and port. |
| `db_cluster` | Discover an Aurora PostgreSQL cluster endpoint and port. |
| `cluster_endpoint` | `writer` (default) or `reader`; requires `db_cluster`. |
| `host` | Explicit endpoint. Set exactly one of `db_instance`, `db_cluster`, or `host`. |
| `port` | Remote port for an explicit host; default `5432`. |
| `database`, `user` | Existing PostgreSQL database and user. |
| `environment` | `development`, `staging`, or `production`; production prints a warning. |
| `auth` | `iam` (default) or `secrets-manager`. |
| `secret_id` | Secret name or ARN; required for `secrets-manager`. |
| `target` | Explicit SSM managed instance ID. |
| `jump_tag` | Alternative EC2 `Name` tag; must match one running instance. |
| `region`, `aws_profile` | Overrides for the standard AWS configuration. |
| `local_port` | Local port; default `0` selects an available port. |
| `sslrootcert` | Custom CA PEM file; required for an explicit `host`. |

For RDS instances and Aurora clusters, pg-tunnel downloads and caches the official
[AWS RDS CA bundle](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.SSL.html)
automatically and checks for updates after 30 days. TLS always uses `verify-full`.
Set `sslrootcert` to manage certificates yourself; custom files are not updated.

## Aurora cluster endpoints

`init` lists individual instances. To use a cluster endpoint, edit your connection:

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

Omit `cluster_endpoint` or use `writer` for the primary. The endpoint is discovered
again for each new session. A reader endpoint can route to the primary when no
replicas exist, so use database permissions to enforce read-only access.
For Secrets Manager authentication, any `host` in the secret must match the chosen
endpoint; a writer-host secret cannot be used for a reader connection.

## AWS access

The SSM host must reach the database and support
`AWS-StartPortForwardingSessionToRemoteHost`. Your AWS identity needs:

| Operation | Permissions |
| --- | --- |
| Open and close tunnels | `ssm:StartSession`, `ssm:TerminateSession` |
| Confirm an already-ended session after a termination error | `ssm:DescribeSessions` |
| Recover an interrupted tunnel | `ssm:ResumeSession` |
| Find an RDS instance | `rds:DescribeDBInstances` |
| Find an Aurora cluster | `rds:DescribeDBClusters` |
| Find a host by `jump_tag` | `ec2:DescribeInstances` |
| Discover hosts in `init` | `ec2:DescribeInstances`, `ssm:DescribeInstanceInformation` |
| IAM database login | `rds-db:connect` |
| Secrets Manager login | `secretsmanager:GetSecretValue`, plus `kms:Decrypt` for a customer-managed KMS key |

IAM authentication also requires IAM enabled on the instance or cluster and
`rds_iam` granted to the database user. PostgreSQL roles determine SQL access;
pg-tunnel does not grant permissions.

For AWS Vault, omit `aws_profile` from the connection and use its credential server:

```sh
aws-vault exec --server YOUR_PROFILE -- pg-tunnel run development -- psql
```

Temporary AWS credentials in environment variables cannot refresh themselves.
Use a renewable credential source for long sessions; SSO may require another login
when its session expires. `AWS_CREDENTIAL_EXPIRATION` lets pg-tunnel report the
expiry of environment credentials.

## Secrets Manager passwords

Choose **Secrets Manager password** in `init`, or add these fields to a connection:

```json
{
  "auth": "secrets-manager",
  "secret_id": "application/reader"
}
```

The secret's JSON `SecretString` must contain `username` and `password`.
The username must match the configured user; optional `host` and `port` must match
the database endpoint, and optional `engine` must be `postgres`. The config's
`database` selects the database. Binary secrets and alternating-user rotation are
not supported; only the secret identifier is saved in the config.

Users with `rds_iam` membership must use IAM authentication:
[IAM takes precedence over password authentication on RDS](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.html).

## Connection warnings

Set `"environment": "production"` to get a startup reminder. pg-tunnel also warns
about privileged database roles, including `rds_superuser`. These are reminders,
not access controls; missing warnings do not guarantee read-only access.

## Client behavior

`run` verifies database access before starting your command. It replaces inherited
`PG*` settings with `PGSERVICE`, `PGSERVICEFILE` and `PGPASSFILE` pointing to private
connection files. Client output stays on stdout; tunnel diagnostics go to stderr.
The local listener uses `127.0.0.1`, while TLS verifies the real database hostname.
Other local users can reach the listener but still need database credentials.

See the [compatibility list](compatibility.md) for supported clients. Explicit
connection strings can override these settings; `DATABASE_URL` is not rewritten.
Go pgx pools need the [connection hook](compatibility.md#go-pgx-pools).

Launch a notebook server through `pg-tunnel run development -- uv run jupyter lab`,
then connect from a notebook:

```python
import psycopg

conn = psycopg.connect("")
```

Notebooks in one Lab server share a tunnel. Each separately launched Lab server,
such as one per worktree, gets its own tunnel and credentials. Leave `local_port`
unset or `0` to avoid port conflicts. Restarting a notebook leaves the tunnel
running; shutting down its server closes it. Already-running servers do not
inherit the connection settings.

For a separately launched client:

```sh
pg-tunnel connect development
```

Copy its printed shell exports to another terminal, or configure your client's
service and password file paths. Keep `connect` running; Ctrl-C closes it and
exits 0. GUI support depends on the [client](compatibility.md).

See [network recovery](recovery.md) and the [container recipe](containers.md)
for those environments.

## Renewal and cleanup

IAM tokens refresh three minutes before expiry. Secrets Manager passwords are
reread from `AWSCURRENT` every five minutes, so new connections may briefly fail
after a password rotation. Refreshed credentials must pass a database login before
replacing the password file; failures retain the previous credentials and retry.
Token expiry affects new logins, not established connections.

Credentials stay in private files (0600) inside a private directory (0700).
The shared `~/.pgpass` is never modified. Shutdown stops the client and tunnel,
removes temporary credentials, and requests remote session termination.

`run` preserves the client command's exit status; its own errors exit 1 and usage
errors exit 2. Signals are forwarded to the client; Ctrl-Z and `fg` suspend and
resume both processes.

After a crash or `SIGKILL`, abandoned files are removed by the next session or by:

```sh
pg-tunnel cleanup
```

Secrets Manager passwords do not expire automatically, so clean up after an
uncatchable termination. The client's own descendants may need separate termination.
