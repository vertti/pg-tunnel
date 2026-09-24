<img src="docs/assets/pg-tunnel-logo.png" alt="pg-tunnel logo" width="240">

# pg-tunnel

Connect PostgreSQL clients to private AWS RDS databases through SSM, with
automatically refreshed IAM tokens or Secrets Manager passwords. Runs on macOS and Linux as a single
executable, with no separate SSM plugin to install.

```sh
pg-tunnel run development -- psql
pg-tunnel run development -- python analysis.py
pg-tunnel run development -- uv run jupyter lab
```

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
