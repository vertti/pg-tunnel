# Network interruptions

When an SSM connection breaks, pg-tunnel tries to resume it for up to one minute
after detecting the disconnect. Your client keeps running with the same tunnel
port and connection settings. Recovery requires `ssm:ResumeSession` permission.

If recovery succeeds, new database connections can use the tunnel again.
Interrupted queries or transactions may still fail; pg-tunnel never replays SQL.
If recovery times out, authorization is rejected, or the SSM session no longer
exists, pg-tunnel stops the client command and removes its temporary credentials.
Remote cleanup errors are reported if AWS cannot confirm termination.
If nonrenewable AWS credentials expire, renew AWS access to terminate any
remaining remote session.

Recovery is verified with read-only notebook queries and two-minute laptop sleep
on macOS. If connections continue to fail, save your work before restarting
pg-tunnel and Jupyter.

Maintainers can run the [live recovery checks](development.md#live-checks).
