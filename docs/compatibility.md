# Client compatibility

pg-tunnel supplies a local TCP tunnel and private PostgreSQL connection files.
Clients must route to the tunnel, verify the **real database hostname**, and read
the current password for each new connection. Supporting `.pgpass` alone does not
establish all three.

Last updated: **2026-09-25**. New renewal checks use pg-tunnel **v0.2.1** on
macOS arm64. Historical notebook results are from **2026-09-22**. “Not tested” means no completed verification, not an incompatibility.
A successful existing connection does not prove credential renewal. The live run
verified refreshed-file logins, but did **not** establish the exact server-side
rejection time for the original token; see the [evidence](live-acceptance.md#client-compatibility-follow-up).

| Client / tested version | Setup | Verified TLS login | Fresh login after IAM expiry | Evidence / limits |
| --- | --- | --- | --- | --- |
| psql 18.6 | `pg-tunnel run development -- psql` | Passed live | Passed live | [Renewal evidence](live-acceptance.md#client-compatibility-follow-up) |
| psycopg2 2.9.13 | Inherited settings; `psycopg2.connect("")` | Passed live | Passed live | Also [previously verified with 2.9.12](live-acceptance.md#token-renewal-follow-up). |
| Psycopg 3.3.6 | Inherited settings; `psycopg.connect("")` | Passed live | Passed live | [Renewal evidence](live-acceptance.md#client-compatibility-follow-up) |
| SQLAlchemy 2.1.0 + psycopg2 2.9.13 | `create_engine("postgresql+psycopg2://", pool_pre_ping=True)` | Passed live | Passed live after `engine.dispose()` | Avoid embedding a password in the URL. |
| JupyterLab 4.6.3 / Server 2.21.0–2.21.1 + psycopg2 | Launch the whole server through `pg-tunnel run` | Passed live | Not separately tested in Jupyter | Two kernels, cell interrupt, restart and shutdown verified. [Network interruption limits](recovery.md); [setup](usage.md#client-behavior). |
| Go pgx / pgxpool 5.11.0 | [Connection hook below](#go-pgx-pools) required | Passed locally and live | Passed live, two pools at 15m31s | [Renewal evidence and limits](live-acceptance.md#client-compatibility-follow-up) |
| psql 18.6 inside Docker Desktop 29.8.0 (macOS arm64 host, Linux arm64 container) | [Same-container recipe](containers.md) | Passed live | Passed live at 15m31s | Source build `820203d`; wrong TLS hostname rejected, local files/listener removed. Linux host and editor-managed devcontainer not tested. |
| pgJDBC 42.7.13 / Java 17.0.18 | Generated service file is not directly compatible | Loopback URL rejected, as expected | Not tested | [Reproduced limitations below](#dbeaver-and-jdbc). |
| DBeaver 26.2.1 (source reviewed only) | Requires a verified JDBC routing/TLS recipe | GUI not tested | Not tested | PgPass authentication alone does not resolve the JDBC limitation. |

Unlisted clients, including DataGrip, pgAdmin, asyncpg and node-postgres, have not
been verified. Explicit connection strings can override inherited settings;
pg-tunnel does not generate or update `DATABASE_URL` or `PGPASSWORD`.

## Database endpoints

| Endpoint | Configuration | Verification |
| --- | --- | --- |
| RDS PostgreSQL instance | `db_instance` | Live TLS login, IAM renewal and Secrets Manager authentication; see [acceptance evidence](live-acceptance.md). |
| Aurora PostgreSQL instance | `db_instance` | Resolver fixtures; no live Aurora connection verified. |
| Aurora PostgreSQL cluster writer / reader | `db_cluster`, optional `cluster_endpoint` | Local fixtures verify selection, fresh discovery, forwarding parameters, IAM signing and TLS/client settings. No live Aurora login or failover test. |

Cluster fixture checks use this source checkout; v0.2.1 does not include cluster
selection. Cluster selection currently requires [manual configuration](usage.md#aurora-cluster-endpoints).
The repeatable local check is `mise x -- go test ./internal/awsdb -run TestCluster -v`.
The changed-endpoint fixture verifies a fresh API lookup, not an actual Aurora
failover. A live acceptance check still requires a fresh TLS-verified read-only
identity query through each chosen endpoint and cleanup afterward. No Aurora
PostgreSQL cluster was available in the existing test account/region on 2026-09-25.

## Go pgx pools

pgx 5.11.0 does not implement libpq's `hostaddr`: it treats it as a server runtime
parameter. It also reads the password file when parsing configuration, so a pool
can retain an expired token. The [example configuration](../examples/pgxpool/config.go)
uses pgx's existing `LookupFunc` and `BeforeConnect` hooks to solve both.

Copy that file into your application, adjust its package name, and use `Config()`
in place of `pgxpool.ParseConfig("")`. Then create the pool normally:

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

Launch the application with `pg-tunnel run development -- go run .` (or its built
executable). Set application pool options after calling `Config`; retain its
`BeforeConnect` hook. Each pool captures its own password-file path and rereads
that file on every physical connection. A missing file or credential fails
explicitly. The real host remains the TLS identity and password lookup key;
only address resolution is redirected to loopback.

The example is copyable application code, not a supported Go SDK. It adds no
code or dependency to the pg-tunnel release binary. pgxpool's `puddle` dependency
is used only by the example and its tests.

**Smallest repeatable test:** with PostgreSQL installed, run from this repository:

```sh
mise x -- sh -c 'PATH="$(pg_config --bindir):$PATH" go test ./internal/libpq -run TestPgxPoolExampleWithPostgres -v -count=1'
```

The test uses a disposable local PostgreSQL server (PostgreSQL tools must be
installed; CI requires them): replace its password and the
private credential file, reset the pool, and verify a different backend PID can
log in. The old password and a wrong TLS hostname must fail. A second session's
file must remain independent. This runs in CI; it does not simulate IAM expiry.

## DBeaver and JDBC

DBeaver has [PgPass authentication](https://dbeaver.com/docs/dbeaver/Authentication-PostgreSQL-Pgpass/).
In [26.2.1's implementation](https://github.com/dbeaver/dbeaver/blob/26.2.1/plugins/org.jkiss.dbeaver.ext.postgresql/src/org/jkiss/dbeaver/ext/postgresql/model/AuthModelPgPass.java),
`PGPASSFILE` selects the password file and “Override host name” changes the
password lookup. That setting does not change the TLS hostname or socket routing.
A separately launched application also needs the session's file settings.

A headless check with the official pgJDBC 42.7.13 driver reproduced two barriers:

- `Driver.parseURL("jdbc:postgresql:///?service=pg-tunnel", new Properties())`
  rejects the generated service file, which includes unsupported `hostaddr`.
- Reading the password from `PGPASSFILE` works, but connecting to
  `jdbc:postgresql://127.0.0.1:TUNNEL_PORT/DATABASE` with the RDS CA and
  `sslmode=verify-full` fails hostname verification.

These are JDBC results, **not a completed DBeaver GUI test**. A working recipe
still needs to separate the socket address from the verified database hostname,
and prove that reconnecting reads the refreshed password. Do not disable TLS
hostname verification to get a green connection indicator.

## Maintaining this list

Update the table alongside client fixes and recipes. Record the client/driver
versions, pg-tunnel version or commit, OS, date and the exact setup tested. Keep
local fixtures, source review and live tests distinct.

For each new compatibility claim, the smallest concrete acceptance check is:

1. Open a connection and run a read-only identity query with verified TLS.
2. Keep loopback routing unchanged and check that a deliberately wrong TLS
   hostname fails certificate verification (a DNS failure does not count).
3. Wait past the original IAM token's expiry, close/reset the connection, then
   open a genuinely new connection and repeat the query. Record its new backend
   PID; an unchanged connection does not count. For password authentication,
   rotate credentials only in a disposable test database.
4. Repeat with two simultaneous pg-tunnel processes: ports and private files must
   differ, and stopping one must leave the other usable.

Record partial results as partial; a planned check is not a pass.
