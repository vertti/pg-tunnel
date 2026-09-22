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
