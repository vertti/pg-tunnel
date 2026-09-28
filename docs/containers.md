# Docker and devcontainers

Run **pg-tunnel and your client in the same container**. The tunnel listens on
that container's loopback interface, and its private connection files live there.

In an existing devcontainer, install the Linux pg-tunnel binary for its architecture,
`ca-certificates`, and your client. Configure working AWS credentials **inside**
the container, then launch the client there:

```sh
pg-tunnel run --config /workspace/pg-tunnel.json development -- psql
# Or start your application / notebook server through the same command.
```

Use container paths for configuration and custom certificates. AWS credentials
must also work inside the container: a host AWS Vault keychain or credential
server is not automatically accessible there. For long sessions, configure a
renewable credential source inside the container.

## A finite AWS Vault session

For a local Docker daemon, mount a temporary AWS Vault export read-only.
Database tokens renew until the exported AWS credentials expire; then restart
with a fresh export. Credentials stay out of the image and Docker's environment.

In the directory holding your `pg-tunnel.json`, download the Linux binary for
Docker's architecture and build a client image with HTTPS trust certificates (the
base PostgreSQL image does not include them):

```sh
archive="pg-tunnel_linux_$(docker version --format '{{.Server.Arch}}').tar.xz"
release=https://github.com/vertti/pg-tunnel/releases/latest/download
curl -fsSLO "$release/$archive" -O "$release/checksums.txt"
grep " $archive\$" checksums.txt | shasum -a 256 -c -
mkdir -p bin && tar -xf "$archive" -O pg-tunnel > bin/pg-tunnel-linux && chmod 755 bin/pg-tunnel-linux

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

See the [compatibility list](compatibility.md) for tested environments and
[developer checks](development.md#live-checks) for renewal testing.
