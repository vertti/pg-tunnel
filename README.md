# pg-tunnel

A planned developer utility for connecting PostgreSQL clients to private databases,
starting with AWS RDS and Systems Manager Session Manager.

The intended workflow manages database discovery, tunneling, credentials, IAM
token refresh, client configuration, and cleanup for a command or interactive
session.

Status: initial repository setup. No implementation yet.

## Development setup

Install [mise](https://mise.jdx.dev/getting-started.html), then run:

```sh
mise trust
mise install
mise exec -- go version
```

Development tools are managed through `mise.toml`, with exact versions pinned
for reproducibility. Run Go commands through `mise exec -- go ...`, or activate
mise in your shell. Add future development tools to the same configuration.
