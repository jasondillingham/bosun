# Installing bosun

Bosun is a single static Go binary. Pick whichever install path matches how you usually pick up CLIs.

## Prebuilt binary (recommended)

Tagged releases publish darwin / linux / windows × amd64 / arm64 archives on the [Releases page](https://github.com/jasondillingham/bosun/releases/latest), plus a `checksums.txt` covering all of them. The installer scripts below resolve the latest version, verify the SHA-256, extract the `bosun` binary, and drop it on `PATH`.

**macOS / Linux:**

```sh
curl -fsSL https://raw.githubusercontent.com/jasondillingham/bosun/main/scripts/install.sh | sh
```

Defaults: latest version, `/usr/local/bin` if writable else `$HOME/.local/bin`. Override either:

```sh
curl -fsSL .../install.sh | BOSUN_VERSION=v0.11.0 sh
curl -fsSL .../install.sh | BOSUN_INSTALL_DIR=$HOME/bin sh
```

**Windows (PowerShell):**

```powershell
iwr -useb https://raw.githubusercontent.com/jasondillingham/bosun/main/scripts/install.ps1 | iex
```

Defaults: latest version, `$HOME\bin` (added to the user PATH automatically). Override:

```powershell
$env:BOSUN_VERSION = 'v0.11.0'; iwr ... | iex
$env:BOSUN_INSTALL_DIR = "$HOME\.local\bin"; iwr ... | iex
```

**Don't want to pipe a script into a shell?** Both installers are short ([`scripts/install.sh`](../scripts/install.sh), [`scripts/install.ps1`](../scripts/install.ps1)) — read them first, then either run locally or grab the archive directly from the [Releases page](https://github.com/jasondillingham/bosun/releases/latest) and extract by hand:

```sh
# Manual install, macOS Apple Silicon example
curl -fsSL -O https://github.com/jasondillingham/bosun/releases/download/v0.11.0/bosun_0.11.0_darwin_arm64.tar.gz
tar -xzf bosun_0.11.0_darwin_arm64.tar.gz bosun
install -m 0755 bosun /usr/local/bin/bosun
```

## go install

If you already have Go 1.25+ on PATH:

```sh
go install github.com/jasondillingham/bosun/cmd/bosun@latest
```

Drops `bosun` into `$(go env GOBIN)` (or `$(go env GOPATH)/bin`). The version reported by `bosun --version` will be the module pseudo-version (e.g. `v0.11.0`); for the same git-describe formatting the Makefile uses, build from source instead.

## Build from source

```sh
git clone https://github.com/jasondillingham/bosun.git
cd bosun
make build      # produces ./bosun in the repo root
make install    # go install into $GOPATH/bin
```

`make build` injects the git-describe version (`v0.11.0-7-gc2785aa`, etc.) into the binary via `-ldflags "-X main.version=…"` — same pattern GoReleaser uses for tagged builds.

## Homebrew

Not available yet. A `brew install jasondillingham/bosun/bosun` formula is planned once a dedicated tap repo lands; tracking lives as a TODO in [`.github/workflows/release.yml`](../.github/workflows/release.yml).

## Verifying a release

Every release ships a `checksums.txt` alongside the archives. Verify by hand:

```sh
curl -fsSL -O https://github.com/jasondillingham/bosun/releases/download/v0.11.0/checksums.txt
sha256sum --check --ignore-missing checksums.txt   # Linux
shasum -a 256 -c checksums.txt --ignore-missing    # macOS
```

The installer scripts do this automatically before extracting the archive.

## Requirements

- Git on PATH (≥ 2.40)
- macOS, Linux, or Windows on amd64 / arm64

Go is **not** required at runtime — only if you're building from source or using `go install`.
