# Client compatibility

Clients need to use the local tunnel, verify the database's real TLS hostname,
and reread credentials for new connections. pg-tunnel supplies libpq service and
password files; it does not set `PGPASSWORD` or rewrite `DATABASE_URL`.

| Client / tested version | Setup | Connection | IAM renewal |
| --- | --- | --- | --- |
| psql 18.6 | `pg-tunnel run development -- psql` | Works | Works |
| psycopg2 2.9.13 | `psycopg2.connect("")` | Works | Works |
| Psycopg 3.3.6 | `psycopg.connect("")` | Works | Works |
| SQLAlchemy 2.1.0 + psycopg2 2.9.13 | `create_engine("postgresql+psycopg2://", pool_pre_ping=True)` | Works | Works after `engine.dispose()` |
| JupyterLab 4.6.3 + psycopg2 | [Launch the server through pg-tunnel](usage.md#client-behavior) | Works | Not separately tested in Jupyter |
| Go pgx / pgxpool 5.11.0 | [Connection hook below](#go-pgx-pools) | Works with hook | Works with hook |
| psql 18.6 in Docker Desktop | [Run both in the same container](containers.md) | Works | Works |
| pgJDBC 42.7.13 | [JDBC limitations below](#dbeaver-and-jdbc) | No working recipe | Not tested |
| DBeaver 26.2.1 | Uses JDBC | GUI not tested | Not tested |

These results cover macOS arm64 and a Linux arm64 container on Docker Desktop.
Linux hosts, editor-managed devcontainers and unlisted clients have not been
verified. Explicit connection strings can override the inherited settings.

## Database endpoints

| Endpoint | Configuration | Status |
| --- | --- | --- |
| RDS PostgreSQL instance | `db_instance` | Verified with IAM and Secrets Manager authentication. |
| Aurora PostgreSQL instance | `db_instance` | Automated tests only; live login not verified. |
| Aurora cluster writer / reader | `db_cluster`, `cluster_endpoint` | Automated tests only; live login and failover not verified. |

Cluster selection requires [manual configuration](usage.md#aurora-cluster-endpoints)
and is not included in v0.2.1.

## Go pgx pools

pgx needs two hooks: `LookupFunc` to route through the tunnel while keeping the
real TLS hostname, and `BeforeConnect` to reread the password file on each new
connection. Copy [config.go](../examples/pgxpool/config.go) into your application,
adjust its package name, and use `Config()` instead of `pgxpool.ParseConfig("")`:

```go
config, err := Config()
if err != nil {
    return err
}
pool, err := pgxpool.NewWithConfig(ctx, config)
if err != nil {
    return err
}
defer pool.Close()
return pool.Ping(ctx)
```

Launch with `pg-tunnel run development -- go run .`. Set your pool options after
calling `Config`, and retain both hooks so new connections use fresh credentials.

## DBeaver and JDBC

pgJDBC does not support the generated service file's `hostaddr` setting. Using
`localhost` in a JDBC URL instead fails TLS hostname verification against the RDS
certificate. DBeaver's [PgPass authentication](https://dbeaver.com/docs/dbeaver/Authentication-PostgreSQL-Pgpass/)
does not solve this routing problem; there is no verified recipe for it yet.
Keep TLS hostname verification enabled.
