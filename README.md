<img src="docs/assets/pg-tunnel-logo.png" alt="pg-tunnel logo" width="240">

# pg-tunnel

[![CI](https://github.com/vertti/pg-tunnel/actions/workflows/ci.yml/badge.svg)](https://github.com/vertti/pg-tunnel/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/vertti/pg-tunnel)](https://github.com/vertti/pg-tunnel/releases/latest)
[![Go version](https://img.shields.io/github/go-mod/go-version/vertti/pg-tunnel)](go.mod)
[![Platforms](https://img.shields.io/badge/platform-macOS%20%7C%20Linux-lightgrey)](docs/install.md)
[![License](https://img.shields.io/github/license/vertti/pg-tunnel)](LICENSE)

Connect PostgreSQL clients to private AWS RDS or Aurora databases:

```sh
pg-tunnel run development -- psql
pg-tunnel run development -- python analysis.py
pg-tunnel run development -- uv run jupyter lab
```

pg-tunnel opens an SSM tunnel through a jump host, logs in with an IAM token or
a Secrets Manager password, and starts your command with its connection already
set up. It manages TLS certificates and credential renewal, then removes the
tunnel and private credentials when your command exits.

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

`init` saves connections in your user configuration. For project-specific settings,
start from [`pg-tunnel.example.json`](pg-tunnel.example.json); see
[configuration and AWS permissions](docs/usage.md).

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
separately launched clients, and the [compatibility list](docs/compatibility.md)
for tested clients, credential renewal and known limitations.

## Development

```sh
mise run fmt
mise run ci
```

[Configuration and usage](docs/usage.md) · [Contributor notes](docs/development.md)

Licensed under [Apache-2.0](LICENSE). Third-party components retain their own
licenses and notices.
