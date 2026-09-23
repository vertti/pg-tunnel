# Runtime footprint

Measured on macOS arm64 with Go 1.27.1 on 2026-09-22. These are release artifacts,
not SDK downloads or compiler caches. Go is the selected implementation language;
no Rust comparison has been performed.

| Artifact | Bytes | Approximate MiB |
| --- | ---: | ---: |
| Current utility, including official AWS SSM code, stripped | 11,923,506 | 11.37 |
| Current utility, gzip | 4,508,853 | 4.30 |
| Previous utility, requiring an external plugin, stripped | 10,209,218 | 9.74 |
| Previously required SSM plugin 1.2.835.0 | 9,887,008 | 9.43 |
| Initial AWS SDK probe, stripped | 10,206,546 | 9.73 |

The current runtime is one executable. Embedding AWS's port forwarding code adds
roughly 1.63 MiB to our binary and removes the separate 9.43 MiB plugin executable.
It starts a second process from the same binary to isolate upstream signal and
exit behavior; there is no executable extraction or first-run download. The CA
bundle and operating-system libraries are not included in these measurements.
Include `third_party/session-manager-plugin` license/notice files in release
packages. Other platforms may have different footprints.

The initial SDK probe retained RDS and EC2 discovery, SSM StartSession, Secrets
Manager GetSecretValue, standard configuration/credential loading, and RDS IAM
signing. No AWS calls were executed during that probe. The utility currently uses
IAM only and does not retain Secrets Manager. The embedded plugin adds its own
transport and KMS support dependencies.

The target is an uncompressed utility below 25 MiB. This is a development budget,
not a claim about unmeasured release platforms. Developer tools and Go module
caches are not runtime dependencies.

Reproduce the utility measurement after any change:

```sh
mise run build
wc -c < bin/pg-tunnel
gzip -c bin/pg-tunnel | wc -c
```

Build flags: `go build -trimpath -ldflags="-s -w"`.

## Release packages

Measured on 2026-09-23 with the pinned GoReleaser toolchain, cross-compiled from
macOS arm64 with CGO disabled. Archives include dependency licenses and notices.

| Target | Binary MiB | Archive MiB |
| --- | ---: | ---: |
| macOS arm64 | 11.61 | 4.48 |
| macOS amd64 | 12.60 | 4.86 |
| Linux arm64 | 11.31 | 4.30 |
| Linux amd64 | 12.32 | 4.77 |

`mise run package` reports exact sizes for each build. Two consecutive local
builds produced identical archive SHA-256 checksums. The release workflow checks
the 25 MiB executable limit and runs each archive on its native platform.
