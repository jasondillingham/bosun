#!/usr/bin/env sh
# Install the latest published bosun binary on macOS or Linux.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/jasondillingham/bosun/main/scripts/install.sh | sh
#   curl -fsSL .../install.sh | BOSUN_VERSION=v0.11.0 sh        # pin a version
#   curl -fsSL .../install.sh | BOSUN_INSTALL_DIR=$HOME/bin sh  # custom dest
#
# Reads the latest release from the GitHub API (or honors BOSUN_VERSION),
# picks the archive matching the host's OS/arch, verifies the SHA-256
# against the published checksums.txt, and drops `bosun` into
# BOSUN_INSTALL_DIR (defaults to /usr/local/bin, falls back to $HOME/.local/bin
# if /usr/local/bin isn't writable).
set -eu

REPO="jasondillingham/bosun"
INSTALL_DIR="${BOSUN_INSTALL_DIR:-}"
VERSION="${BOSUN_VERSION:-}"

err() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

need() {
    command -v "$1" >/dev/null 2>&1 || err "missing required tool: $1"
}

need curl
need tar
need uname

OS_RAW="$(uname -s)"
case "$OS_RAW" in
    Darwin) OS=darwin ;;
    Linux)  OS=linux  ;;
    *)      err "unsupported OS: $OS_RAW (use Windows PowerShell installer, or build from source)" ;;
esac

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
    x86_64|amd64) ARCH=amd64 ;;
    arm64|aarch64) ARCH=arm64 ;;
    *) err "unsupported architecture: $ARCH_RAW" ;;
esac

if [ -z "$VERSION" ]; then
    # Resolve the latest tag via the GitHub redirect (no API token needed,
    # avoids the unauthenticated rate limit on /releases/latest JSON).
    VERSION="$(curl -fsSLI -o /dev/null -w '%{url_effective}' \
        "https://github.com/$REPO/releases/latest" \
        | sed 's:.*/::')"
    [ -n "$VERSION" ] || err "could not resolve latest version"
fi

# GoReleaser drops the leading `v` from the version inside archive names
# (see name_template in .goreleaser.yaml) but the tag still carries it.
VERSION_NO_V="${VERSION#v}"
ARCHIVE="bosun_${VERSION_NO_V}_${OS}_${ARCH}.tar.gz"
BASE="https://github.com/$REPO/releases/download/$VERSION"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

printf 'Downloading %s ...\n' "$ARCHIVE"
curl -fsSL -o "$TMPDIR/$ARCHIVE"      "$BASE/$ARCHIVE"
curl -fsSL -o "$TMPDIR/checksums.txt" "$BASE/checksums.txt"

# Verify SHA-256 against the published checksums file before unpacking.
# `shasum -a 256` is on macOS by default; `sha256sum` is the Linux standard.
# We grep the matching line out of checksums.txt and pipe it back through
# the checker so it computes + compares in one step.
( cd "$TMPDIR" && {
    if command -v sha256sum >/dev/null 2>&1; then
        grep " ${ARCHIVE}\$" checksums.txt | sha256sum -c -
    elif command -v shasum >/dev/null 2>&1; then
        grep " ${ARCHIVE}\$" checksums.txt | shasum -a 256 -c -
    else
        err "no sha256 tool found (need sha256sum or shasum)"
    fi
} ) || err "checksum verification failed for $ARCHIVE"

tar -xzf "$TMPDIR/$ARCHIVE" -C "$TMPDIR" bosun

# Pick an install dir: env override > /usr/local/bin if writable > ~/.local/bin.
if [ -z "$INSTALL_DIR" ]; then
    if [ -w /usr/local/bin ] 2>/dev/null; then
        INSTALL_DIR=/usr/local/bin
    else
        INSTALL_DIR="$HOME/.local/bin"
        mkdir -p "$INSTALL_DIR"
    fi
fi

install -m 0755 "$TMPDIR/bosun" "$INSTALL_DIR/bosun"

printf '\nInstalled bosun %s -> %s/bosun\n' "$VERSION" "$INSTALL_DIR"
case ":$PATH:" in
    *":$INSTALL_DIR:"*) : ;;
    *) printf 'Note: %s is not on PATH yet. Add it to your shell rc:\n  export PATH="%s:$PATH"\n' "$INSTALL_DIR" "$INSTALL_DIR" ;;
esac

"$INSTALL_DIR/bosun" --version
