# Live AWS acceptance

Tested on 2026-09-22 from macOS arm64 using the embedded official AWS SSM
implementation in commit `c65b3bb`. AWS Vault supplied credentials through its renewable credential server.

| Check | Result |
| --- | --- |
| EC2 jump-host discovery and RDS endpoint discovery | Passed against existing infrastructure. |
| Embedded SSM remote-host forwarding | Passed without invoking a separate plugin executable. |
| IAM authentication and full TLS certificate verification | Passed in the readiness check and PostgreSQL clients. |
| psql with inherited libpq settings | Connected as the reader to the expected database. |
| PostgreSQL transport | TLS 1.3; server reported `TLS_AES_256_GCM_SHA384`. |
| Python with `psycopg2.connect("")` | Three concurrent connections succeeded, exercising multiplexed forwarding. |
| Read-only queries | Explicit read-only transactions checked identity and connection metadata only. |
| Private client files | Python confirmed mode 0600; files were absent after each completed session. |
| Normal remote cleanup | AWS SSM history confirmed both completed client sessions were terminated. |
| Ctrl-C | pg-tunnel reported status 130 and removed private client files; AWS remained `Terminating` for several minutes and reached `Terminated` after an explicit retry. AWS Vault itself reported status 1 while wrapping the child's 130. |

All three test sessions ultimately reached `Terminated`; no pg-tunnel or plugin
processes remained locally. The delayed termination in this initial run prompted the shutdown investigation
described below.

The following SQL was used with psql, without a user startup file or password
prompt:

```sql
BEGIN READ ONLY;
SELECT current_user, current_database(),
       current_setting('transaction_read_only');
SELECT ssl, version, cipher
FROM pg_stat_ssl
WHERE pid = pg_backend_pid();
ROLLBACK;
```

Python used Python 3.11 and psycopg2 2.9.12,
launched through `mise exec -- uv run --no-sync python`. Each connection set
`readonly=True`, queried its identity and TLS metadata, and closed explicitly.

This initial acceptance run did not exercise long-running token renewal across
the 15-minute token lifetime or AWS credential rotation. Deterministic automated tests cover renewal,
retry, and cancellation. This acceptance run does not establish compatibility
with every SSM agent version, database configuration, or GUI client.

## Shutdown follow-up

The supervisor sends SIGTERM, but upstream's graceful port-forwarding handler
registered only SIGINT, SIGQUIT, and SIGTSTP. The embedded adapter now registers
SIGTERM too, allowing AWS's handler to send its termination flag and remove its
multiplexing socket before exiting.

A local regression test uses upstream message serialization and the actual
embedded handler. It fails on the previous code with an abrupt WebSocket close;
the fix sends the termination flag and removes the socket. A separate test checks
AWS credential-cache renewal and recovery after a temporary provider failure.

Live normal exit, client failure, and SIGINT checks preserved statuses 0, 23,
and 130, removed private files, and reached Terminated within the 30-second
observation window without an extra API retry. The final implementation passed
all three checks again.

Graceful shutdown can finish the session before the fallback TerminateSession
call, causing AWS to reject it as no longer in a valid state. On API failure,
we now query that session's history once and accept only a matching Terminated
record. Missing history, another status, or denied access keeps the cleanup
error visible. This confirmation requires `ssm:DescribeSessions`; it is not
called after successful termination requests. Cleanup retains its existing
five-second deadline and does not poll or repeat successful API calls.

Requesting API termination before stopping the plugin did not resolve the
remote-status delay in live experiments; that ordering was discarded. All
experimental sessions were subsequently confirmed Terminated. The final code
addresses the demonstrated signal mismatch and already-terminated race; a
successful API response alone still means termination was requested, not that
its final cloud status was observed.

## Token renewal follow-up

A live session ran beyond its original 15-minute IAM token lifetime. Fresh
psycopg2 connections succeeded every minute with read-only transactions and
verified TLS. The private password file changed at 12 minutes, retained mode
0600, and a fresh connection succeeded 30 seconds after the original token
expired. Both private client files disappeared on exit, and SSM confirmed the
session terminated.

The run used AWS Vault's renewable credential server, but its underlying AWS
credentials did not expire during the test. Actual AWS credential rotation is
therefore not live-verified. A deterministic test using the real AWS credential
cache covers expiry, transient provider failure, and recovery with replacement
credentials.

## Client compatibility follow-up

On 2026-09-25, the v0.2.1 CLI on macOS arm64 ran two simultaneous read-only
SSM/IAM sessions. The [compatibility list](compatibility.md) records exact client
versions and recipes. Two pgx 5.11.0 pools opened new physical connections every
minute, with different backend PIDs and verified TLS. Their private password
files changed at 12 minutes; fresh logins succeeded at 15m31s, after the original
tokens' advertised expiry. Ports and private files were distinct, with 0600 file
and 0700 directory permissions. Both sessions removed their files and listeners;
AWS SSM history confirmed both reached `Terminated`.

psql 18.6, Psycopg 3.3.6, psycopg2 2.9.13 and SQLAlchemy 2.1.0 with psycopg2 also
opened fresh read-only TLS connections after that expiry. The SQLAlchemy check
disposed the pool first; all checks observed new backend PIDs. pgx rejected a
wrong TLS hostname in both long runs. A separate short session confirmed the
same rejection for all four libpq clients, keeping loopback routing unchanged.

An additional negative assertion expected the original IAM tokens to fail at
15m31s. **RDS still accepted them**, so both Go harnesses exited 1 on that assertion
after the successful renewal checks. This run establishes refreshed-file login
behavior, not the exact server-side expiry cutoff or rejection of stale IAM
tokens. The local PostgreSQL regression test separately proves that the example
reloads a replacement password when the original password no longer works.
Underlying AWS credentials did not expire during this run.
