# Initial runtime footprint

Measured on macOS arm64 with Go 1.27.1 on 2026-09-22. These are release artifacts,
not SDK downloads or compiler caches. Go is the selected implementation language;
no Rust comparison has been performed.

| Artifact | Bytes | Approximate MiB |
| --- | ---: | ---: |
| AWS SDK probe, stripped | 10,206,546 | 9.73 |
| AWS SDK probe, gzip | 3,898,818 | 3.72 |
| First complete utility, stripped | 10,209,218 | 9.74 |
| First complete utility, gzip | 3,918,329 | 3.74 |
| Installed SSM plugin 1.2.835.0 | 9,887,008 | 9.43 |

The SDK probe retained calls for RDS and EC2 discovery, SSM StartSession,
Secrets Manager GetSecretValue, standard configuration/credential loading, and
RDS IAM signing. It was compiled without executing AWS API calls. The actual
utility currently uses IAM only and does not retain Secrets Manager.

The executable plus the measured SSM plugin total roughly 19.2 MiB, excluding
the CA bundle and operating-system libraries. Developer tools and Go module
caches are not runtime dependencies. Plugin packaging and other platforms may
have different footprints.

The initial target is an uncompressed utility below 25 MiB. Keep the SSM plugin
visible as a separate runtime dependency when reporting download/install size.
This is a development budget, not a claim about unmeasured release platforms.

Reproduce the utility measurement after any change:

```sh
mise run build
wc -c < bin/pg-tunnel
gzip -c bin/pg-tunnel | wc -c
mise exec -- sh -c 'wc -c < "$(command -v session-manager-plugin)"'
```

Build flags: `go build -trimpath -ldflags="-s -w"`.
