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

Connecting a laptop safely to a private RDS database takes all of this:

- Find a jump host that SSM can reach and that can reach the database.
- Install the Session Manager plugin and keep a port-forwarding session open.
- Get an IAM token, or read the password from Secrets Manager.
- Replace the token before it expires after 15 minutes, or pick up a rotated password.
- Download the RDS CA bundle and verify the real hostname through `localhost`.
- Keep the password out of environment variables, shell history, and `~/.pgpass`.
- Notice when you're about to touch production or connect as a superuser.
- Close the tunnel, the remote session, and the credentials when you're done.

pg-tunnel does all of it in one command, and `init` finds the database and jump
host for you.

## Setup

Install with [Homebrew](https://brew.sh) or [mise](https://mise.jdx.dev):

```sh
brew install vertti/tap/pg-tunnel
# or
mise use -g github:vertti/pg-tunnel
```

Or download an archive from the
[latest release](https://github.com/vertti/pg-tunnel/releases/latest) and follow
the [installation steps](docs/install.md).

With working AWS credentials, let `init` find your database and jump host, test
the connection, and save it under a connection name:

```sh
pg-tunnel init --aws-profile YOUR_AWS_PROFILE   # or omit it to use your current credentials
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
