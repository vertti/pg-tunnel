<img src="docs/assets/pg-tunnel-logo.png" alt="pg-tunnel logo" width="240">

# pg-tunnel

[![CI](https://github.com/vertti/pg-tunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/vertti/pg-tunnel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/vertti/pg-tunnel)](https://github.com/vertti/pg-tunnel/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/vertti/pg-tunnel)](go.mod)
[![Platforms](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey)](docs/install.md)
[![License](https://img.shields.io/github/license/vertti/pg-tunnel)](LICENSE)

Run psql, a Python script, or Jupyter against a private RDS or Aurora PostgreSQL
database with one command:

```sh
pg-tunnel run development -- psql
pg-tunnel run development -- python analysis.py
pg-tunnel run development -- uv run jupyter lab
```

pg-tunnel opens an SSM tunnel through a jump host, logs in with an IAM token or
a Secrets Manager password, and starts your command with its connection already
set up. When the command exits, the tunnel and the credentials go away.

## Why

Without it, reaching a private RDS database through SSM usually looks like this:

```sh
# Terminal 1: find the jump host and keep a tunnel open
aws ec2 describe-instances --filters Name=tag:Name,Values=db-jump \
  --query 'Reservations[].Instances[].InstanceId' --output text
aws ssm start-session --target i-0123456789abcdef0 \
  --document-name AWS-StartPortForwardingSessionToRemoteHost \
  --parameters host=mydb.abc123.eu-central-1.rds.amazonaws.com,portNumber=5432,localPortNumber=15432

# Terminal 2: mint a token and connect
export PGPASSWORD="$(aws rds generate-db-auth-token --region eu-central-1 \
  --hostname mydb.abc123.eu-central-1.rds.amazonaws.com --port 5432 --username reader)"
psql "host=localhost port=15432 dbname=app user=reader sslmode=require"
```

That works for a quick look, and then it starts to hurt:

- The IAM token expires after 15 minutes. Your open psql session survives, but
  the next notebook reconnect or pool connection fails until you mint a new one.
- `sslmode=require` encrypts but doesn't check who is on the other end. The
  certificate names the RDS host, not `localhost`, so `verify-full` fails unless
  you download the RDS CA bundle and pass the real hostname separately.
- The token sits in `PGPASSWORD`, inherited by everything you start from that
  shell.
- You need the AWS CLI and the separate Session Manager plugin installed, and
  the tunnel keeps running in the other terminal after you're done.

If you've written a wrapper script for this, pg-tunnel is that script with the
edge cases handled:

- One executable with the Session Manager plugin built in. You need the AWS CLI
  only if you log in with it, for example `aws sso login`.
- Finds the RDS endpoint from the instance name and the jump host from its EC2
  `Name` tag, or `init` discovers both and saves them for you.
- Refreshes IAM tokens three minutes before they expire and rereads rotated
  Secrets Manager passwords. Each new credential must pass a real login before
  clients see it.
- Always uses `verify-full` TLS against the real RDS hostname, with the AWS CA
  bundle downloaded and cached.
- Hands credentials to libpq through a private per-session password file, never
  through environment variables, arguments, or your `~/.pgpass`.
- Cleans up on exit, Ctrl-C, or a closed terminal: the command, the tunnel, the
  remote SSM session, and the credential files. Your command's exit code comes
  back unchanged, so it works in scripts and CI.
- Prints a warning when a connection is marked `production` or the database
  user has privileges such as `SUPERUSER` or `rds_superuser`.

## Setup

Download a [release](https://github.com/vertti/pg-tunnel/releases/latest) and
follow the [installation steps](docs/install.md), or build from source with [mise](https://mise.jdx.dev/getting-started.html):

```sh
git clone https://github.com/vertti/pg-tunnel.git
cd pg-tunnel
mise trust
mise install
mise run build   # creates bin/pg-tunnel
```

Log in to AWS, then let `init` find your database and jump host, test the
connection, and save it under a connection name:

```sh
aws sso login --profile YOUR_AWS_PROFILE
pg-tunnel init --aws-profile YOUR_AWS_PROFILE
pg-tunnel run CONNECTION -- psql
```

`init` saves to your user configuration, which every directory shares. To keep
connections with a project instead, start from `pg-tunnel.example.json` and save
it as `pg-tunnel.json` in the project; a project file replaces the user
configuration while you work in that directory. RDS CA certificates are managed
automatically. The SSM jump host must reach the database. See
[configuration and AWS permissions](docs/usage.md) for details.

## Python and notebooks

Launch your script or Jupyter through `pg-tunnel`. Psycopg picks up the connection
settings automatically:

```python
import psycopg

conn = psycopg.connect("")
```

`pg-tunnel` manages private connection files and removes them on shutdown.
Your shared `~/.pgpass` stays untouched. See the
[client guide](docs/usage.md#client-behavior) for existing notebook servers and
separately launched clients.

## Development

```sh
mise run fmt
mise run ci
```

[Configuration and usage](docs/usage.md) · [Contributor notes](docs/development.md)

Licensed under [Apache-2.0](LICENSE). Third-party components retain their own
licenses and notices.
