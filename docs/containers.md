# Docker and devcontainers

Run **pg-tunnel and your client in the same container**. The tunnel listens on
that container's loopback interface, and its private connection files live there.
Launching pg-tunnel on the host and copying its `PG*` settings into a container
does not make either the listener or those paths available inside it.

In an existing devcontainer, install the Linux pg-tunnel binary for its architecture,
`ca-certificates`, and your client. Configure working AWS credentials **inside**
the container, then launch the client there:

```sh
pg-tunnel run --config /workspace/pg-tunnel.json development -- psql
# Or start your application / notebook server through the same command.
```

Use container paths for configuration and any custom `sslrootcert`. A host AWS
profile that runs a credential helper also needs that helper and its authentication
available inside the container. In particular, a host AWS Vault keychain or
`aws-vault exec --server` loopback address is not automatically accessible there.
For long sessions, use a renewable AWS credential source inside the container;
[SSO configuration](https://docs.aws.amazon.com/sdkref/latest/guide/feature-sso-credentials.html)
still requires a valid login session and a writable token cache for refresh.

## A finite AWS Vault session

This example exports temporary AWS credentials to a private file and mounts it
read-only. It keeps credentials out of the image and Docker environment metadata.
IAM database tokens refresh normally **until those AWS credentials expire**;
the exported snapshot cannot renew the AWS session. The standard
[AWS process provider](https://docs.aws.amazon.com/sdkref/latest/guide/feature-process-credentials.html)
rereads this file; it cannot obtain newer credentials from it. Restart with a fresh export
when needed. This example is for a local Docker daemon.

From a pg-tunnel checkout, build a Linux binary and a client image with HTTPS
trust certificates (the base PostgreSQL image does not include them):

```sh
mise x -- env GOOS=linux GOARCH="$(docker version --format '{{.Server.Arch}}')" \
  CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
  -o bin/pg-tunnel-linux ./cmd/pg-tunnel

docker build -t pg-tunnel-client - <<'DOCKERFILE'
FROM postgres:18.6
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
DOCKERFILE
```

Use an existing `pg-tunnel.json` with a `development` connection. Omit its
`aws_profile` so it uses the container's AWS profile below. Leave `local_port`
unset or `0`; omit `sslrootcert` for automatic RDS CA management, or mount your
custom bundle at its configured container path. Change the AWS profile, region
and connection name below to yours:

```sh
(
  set -eu
  umask 077
  session_dir=$(mktemp -d)
  trap 'rm -rf "$session_dir"' EXIT
  aws-vault export --format=json --duration=1h YOUR_AWS_PROFILE > "$session_dir/credentials.json"
  cat > "$session_dir/config" <<'AWS_CONFIG'
[profile container]
region = eu-central-1
credential_process = cat /run/aws/credentials.json
AWS_CONFIG

  docker run --rm --init -it \
    --user "$(id -u):$(id -g)" \
    --tmpfs /tmp:rw,nosuid,nodev,mode=1777 \
    -e HOME=/tmp -e AWS_PROFILE=container -e AWS_CONFIG_FILE=/run/aws/config \
    --mount "type=bind,src=$session_dir,dst=/run/aws,readonly" \
    --mount "type=bind,src=$PWD/bin/pg-tunnel-linux,dst=/usr/local/bin/pg-tunnel,readonly" \
    --mount "type=bind,src=$PWD/pg-tunnel.json,dst=/run/pg-tunnel.json,readonly" \
    --entrypoint pg-tunnel pg-tunnel-client \
    run --config /run/pg-tunnel.json development -- psql
)
```

No database port is published. Each container gets its own loopback tunnel and
private files. Quit `psql` with `\q` to close the tunnel; the shell then removes
the exported AWS credentials. As with any shell trap, an uncatchable termination
cannot run that removal: remove the temporary export yourself after a host crash
or `SIGKILL`.

## Repeat the renewal and cleanup check

Use an IAM connection and AWS credentials with at least 20 minutes remaining.
In the command above, add this mount:

```sh
--mount "type=bind,src=$PWD/scripts/check-container-renewal.sh,dst=/run/check.sh,readonly"
```

Replace the entrypoint, image and command tail with:

```sh
--entrypoint bash pg-tunnel-client \
  /run/check.sh /run/pg-tunnel.json development EXPECTED_DB_USER EXPECTED_DATABASE
```

The [check](../scripts/check-container-renewal.sh) opens a read-only TLS connection,
checks private file permissions, waits 15m31s, requires a changed password file,
and opens a new database backend with the expected identity. After pg-tunnel
exits, it requires the credential directory and listener to be gone while the
container is still running. A failed assertion exits nonzero. It does not claim
that AWS rejects the original token at an exact second, or verify AWS session
renewal, remote SSM termination, host suspend, or cross-container access.

## Verification

Verified on **2026-09-25** with pg-tunnel source build `820203d`, Docker Desktop
29.8.0 on macOS arm64, and a Linux arm64 container running psql 18.6. The base
image was `postgres:18.6` (manifest
`sha256:5a5a84b19854a9ffaa54082c166ff4ec27473a361e496e5ea167f298f2da9722`),
with Debian's `ca-certificates` package added.

The check passed: initial read-only TLS login, private file permissions, automatic
IAM refresh, a fresh backend after 15m31s with a changed password file, and removal
of the credential directory and listener. A separate check rejected a deliberately
wrong TLS hostname through the same loopback tunnel. The container ran as a
non-root user with no published ports, read-only bind mounts and no AWS credential
or database password environment variables. Its host credential export was removed
afterward. A separate read-only AWS status audit confirmed this test's remote
SSM session reached `Terminated`.

Linux hosts, a full editor-managed devcontainer and underlying AWS credential
renewal have **not** been tested. See the [compatibility list](compatibility.md)
for other clients and their limits.
