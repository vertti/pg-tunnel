<img src="docs/assets/pg-tunnel-logo.png" alt="pg-tunnel logo" width="240">

# pg-tunnel

Connect PostgreSQL clients to private AWS RDS databases through SSM, with
automatically refreshed IAM credentials. Runs on macOS and Linux as a single
executable, with no separate SSM plugin to install.

```sh
pg-tunnel run development -- psql
pg-tunnel run development -- python analysis.py
pg-tunnel run development -- uv run jupyter lab
```

## Setup

Build from source with [mise](https://mise.jdx.dev/getting-started.html):

```sh
git clone https://github.com/vertti/pg-tunnel.git
cd pg-tunnel
mise trust
mise install
mise run build
cp pg-tunnel.example.json pg-tunnel.json
```

Edit `pg-tunnel.json` with your database, IAM database user, SSM jump host, and
AWS profile, or use `./bin/pg-tunnel init --profile YOUR_AWS_PROFILE`. RDS CA
certificates are managed automatically. Authenticate to AWS, then run:

```sh
./bin/pg-tunnel run development -- psql
```

The database must have IAM authentication enabled, and the SSM jump host must
reach it. See [configuration and AWS permissions](docs/usage.md) for details.

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
