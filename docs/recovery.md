# Network interruptions

The embedded AWS plugin attempts to resume an interrupted SSM data connection.
That does **not** guarantee that every outage recovers or that an existing SQL
connection or transaction survives.

## Verified behavior

On 2026-09-25, using the CLI at `fcd5f5f` on macOS arm64 with JupyterLab 4.6.3, Jupyter Server 2.21.1, ipykernel 7.3.0,
psycopg2 2.9.13 and Python 3.11.12:

| Interruption | Observed result |
| --- | --- |
| One disconnect of an idle SSM WebSocket; reconnect allowed immediately | Recovered. The same Jupyter server and kernel opened a fresh read-only TLS connection. Their PIDs, tunnel port and private connection-file paths stayed unchanged; the database backend PID changed. The reusable check took 1.9 seconds from the cut to successful verification. |
| Disconnect followed by five seconds rejecting new SSM data connections | The reusable settled-idle check recovered in 8.7 seconds with unchanged processes/settings and a new database backend. An earlier exploratory run without the settling pauses did not reconnect within 90 seconds; this is not a reliable outage threshold. |
| Disconnect followed by 30 seconds rejecting new SSM data connections | Failed: six reconnect attempts were rejected, with no replacement connection within 90 seconds. The same Jupyter server and kernel stayed alive and the server API still responded. The test then shut them down and verified local cleanup. |

The 30-second check exits nonzero on the current implementation. These are
observations from particular runs, not timing guarantees. The test
lets an idle connection settle before interruption and allows one second after a
replacement CONNECT socket opens before querying. It does not establish recovery
for writes during reconnect, interrupted transactions, laptop sleep, AWS credential
expiry, or deliberate SSM session expiry. Those need separate checks.

The upstream [resume handler](https://github.com/aws/session-manager-plugin/blob/930a08e65d3a/src/sessionmanagerplugin/session/sessionhandler.go)
uses a finite retry loop. pg-tunnel currently supervises the plugin process;
it does not create a replacement SSM session after those retries fail. A live
plugin process and listening local port alone do not prove the database is usable.
If the plugin exits, pg-tunnel stops its child command and cleans up.

Recovery also needs `ssm:ResumeSession` permission. If fresh connections keep
failing, save notebook work before restarting the pg-tunnel/Jupyter command.
pg-tunnel does not replay SQL.

## Repeat the check

Use an existing connection with read-only database access and working AWS
credentials. From a checkout of this repository:

```sh
mise run build
mise x uv@0.12.10 -- uv run scripts/check-jupyter-recovery.py \
  --config /path/to/pg-tunnel.json reader

# Characterize an outage that lasts beyond the initial disconnect:
mise x uv@0.12.10 -- uv run scripts/check-jupyter-recovery.py \
  --config /path/to/pg-tunnel.json --outage 30 reader
```

If you use AWS Vault, run the check inside the same `aws-vault exec --server`
environment as your usual pg-tunnel command. The script installs its test-only
Python dependencies through uv, starts an isolated local Jupyter server and
kernel, and places a temporary CONNECT proxy only in that child's environment.
The proxy relays encrypted bytes without decrypting TLS or recording payloads;
it interrupts only this test's SSM data connection. No system network settings,
existing notebooks, kernelspecs, or database data are changed.

**Pass:** a fresh read-only query succeeds with verified TLS, unchanged server and
kernel PIDs, unchanged port and file paths, and a different PostgreSQL backend
PID. **Fail:** no resumed connection within 90 seconds, a failed fresh query, or
changed process/settings identity. Both outcomes shut down the test and check
that private credentials and the tunnel listener are removed. Versions and private
logs remain in the temporary directory printed by the command. The script checks
local cleanup; it does not query AWS session history.

A failed outage check characterizes a current limitation; it is not a passing
recovery test. This is an opt-in live test, not a CI test or a claim that every
SSM agent/environment behaves identically.
