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
processes remained locally. Delayed remote termination after Ctrl-C remains an
open cleanup issue: this run does not distinguish delayed AWS status reporting
from incomplete termination requiring retry. Reproduce and investigate before
calling remote interruption cleanup fully verified.

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

Long-running token renewal across the 15-minute token lifetime and AWS credential
rotation were not exercised live. Deterministic automated tests cover renewal,
retry, and cancellation. This acceptance run does not establish compatibility
with every SSM agent version, database configuration, or GUI client.
