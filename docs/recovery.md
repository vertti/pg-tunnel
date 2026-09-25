# Network interruptions

When the embedded AWS plugin detects a broken SSM data connection, pg-tunnel
reports the interruption and retries AWS's resume operation for up to one minute.
The client stays running with the same local port and private connection files.
A successful resume is reported, but existing SQL connections or transactions
may have failed. pg-tunnel never replays SQL.

Recovery reuses the existing SSM session. It does not start a replacement session
or bypass session expiry. An explicit authorization rejection or missing session
stops recovery immediately. Network errors and other retryable failures use
backoff, capped at five seconds between attempts. After the budget expires,
pg-tunnel stops its child command and removes its private credentials. Shutdown
has its own short cleanup allowance; remote termination can fail if AWS remains
unreachable, in which case the error is reported.

This budget begins when the plugin detects the disconnect, not when the network
first fails. It does not guarantee recovery from laptop sleep, active writes, or
every SSM agent/environment. Save work before deliberately testing a long outage.

## Baseline before bounded recovery

On 2026-09-25, using the CLI at `fcd5f5f` on macOS arm64 with JupyterLab 4.6.3, Jupyter Server 2.21.1, ipykernel 7.3.0,
psycopg2 2.9.13 and Python 3.11.12:

| Interruption | Observed result |
| --- | --- |
| One disconnect of an idle SSM WebSocket; reconnect allowed immediately | Recovered. The same Jupyter server and kernel opened a fresh read-only TLS connection. Their PIDs, tunnel port and private connection-file paths stayed unchanged; the database backend PID changed. The reusable check took 1.9 seconds from the cut to successful verification. |
| Disconnect followed by five seconds rejecting new SSM data connections | The reusable settled-idle check recovered in 8.7 seconds with unchanged processes/settings and a new database backend. An earlier exploratory run without the settling pauses did not reconnect within 90 seconds; this is not a reliable outage threshold. |
| Disconnect followed by 30 seconds rejecting new SSM data connections | Failed: six reconnect attempts were rejected, with no replacement connection within 90 seconds. The same Jupyter server and kernel stayed alive and the server API still responded. The test then shut them down and verified local cleanup. |

The 30-second check exited nonzero on that baseline. These are
observations from particular runs, not timing guarantees. The test
lets an idle connection settle before interruption and allows one second after a
replacement CONNECT socket opens before querying. It does not establish recovery
for writes during reconnect, interrupted transactions, laptop sleep, AWS credential
expiry, or deliberate SSM session expiry. Those need separate checks.

The upstream [resume handler](https://github.com/aws/session-manager-plugin/blob/930a08e65d3a/src/sessionmanagerplugin/session/sessionhandler.go)
uses a finite retry loop. pg-tunnel replaces that retry callback through AWS's
existing channel interfaces, before the socket starts, and retains AWS's resume,
handshake, signing and forwarding implementation. A live plugin process and
listening local port alone do not prove the database is usable.

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

# Verify recovery after a longer interruption:
mise x uv@0.12.10 -- uv run scripts/check-jupyter-recovery.py \
  --config /path/to/pg-tunnel.json --outage 30 reader

# Verify that recovery exhaustion stops the disposable kernel and cleans up:
mise x uv@0.12.10 -- uv run scripts/check-jupyter-recovery.py \
  --config /path/to/pg-tunnel.json --outage 75 --expect-stop reader
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

With `--expect-stop`, success instead requires a nonzero exit with an explicit
recovery-budget diagnostic, a stopped kernel, and credential/listener cleanup
within 75 seconds of the cut. Failure of the normal recovery check is never
counted as success. These checks are opt-in, not part of CI.

CI tests the retry budget (including a blocked AWS call), explicit authorization
and expiry errors, the actual AWS resume handler against a terminated-session
response, child exit, and redaction of progress output. Live checks are still
required before claiming that an SSM agent/environment recovers.
