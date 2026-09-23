# Installation

Download your archive and `checksums.txt` from the
[latest release](https://github.com/vertti/pg-tunnel/releases/latest), or
[build from source](../README.md#setup).

| System | Archive |
| --- | --- |
| macOS, Apple Silicon | `pg-tunnel_darwin_arm64.tar.gz` |
| macOS, Intel | `pg-tunnel_darwin_amd64.tar.gz` |
| Linux, ARM64 | `pg-tunnel_linux_arm64.tar.gz` |
| Linux, x86-64 | `pg-tunnel_linux_amd64.tar.gz` |

Download your archive and `checksums.txt` from the same release. For example,
on Apple Silicon, run in the download directory:

```sh
grep ' pg-tunnel_darwin_arm64.tar.gz$' checksums.txt | shasum -a 256 -c -
tar -xzf pg-tunnel_darwin_arm64.tar.gz
mkdir -p ~/.local/bin
install -m 755 pg-tunnel ~/.local/bin/pg-tunnel
```

Keep the extracted licenses/notices with your installation. Add `~/.local/bin`
to your shell's `PATH`, then authenticate to AWS and configure a connection:

```sh
pg-tunnel --version
pg-tunnel init --profile YOUR_AWS_PROFILE
pg-tunnel run YOUR_CONNECTION -- psql
```

No Go toolchain or separate SSM plugin is needed. macOS archives are not yet
Apple-notarized; Gatekeeper may require approval through Privacy & Security.
See [client usage](usage.md) for Python and Jupyter.
