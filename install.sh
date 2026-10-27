#!/bin/sh
# leadkit installer. Downloads a release binary, verifies it, installs it atomically.
#
#   sh install.sh                 # latest release
#   sh install.sh v0.2.0          # a specific tag
#   LEADKIT_BIN=~/bin/leadkit sh install.sh
#
# Prefer reading this before running it to piping it into a shell. The checksum check
# below protects the download; it cannot protect you from the script itself.
set -eu

REPO="${LEADKIT_REPO:-MattGrundy/leadkit}"
# Default target is ~/.claude/leadkit-bin, not /usr/local/bin, because `leadkit init` bakes
# the absolute path of the binary into the hooks and LaunchAgents it writes. Installing
# somewhere stable matters more here than being on PATH; add a symlink for convenience.
BIN="${LEADKIT_BIN:-$HOME/.claude/leadkit-bin}"
TAG="${1:-latest}"

die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

# `mv -f tmp "$BIN"` onto a directory MOVES the temp file inside it and succeeds, so the
# install would report success having replaced nothing. Refuse before doing any work.
[ ! -d "$BIN" ] || die "$BIN is a directory; set LEADKIT_BIN to the file path to install to"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) die "unsupported OS '$os'. Build from source with: go install mgpp.dev/leadkit/cmd/leadkit@latest" ;;
esac
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  x86_64|amd64)  arch=amd64 ;;
  *) die "unsupported architecture '$(uname -m)'" ;;
esac

# Release assets are fetched through gh rather than curl: it already holds credentials, so this
# works whether or not the repository is public, and asking for a token in an env var would be
# worse. Fail with the actual remedy rather than a 404 from curl.
command -v gh >/dev/null 2>&1 || die "needs the gh CLI (brew install gh && gh auth login)"
gh auth status >/dev/null 2>&1 || die "gh is installed but not authenticated — run: gh auth login"

if [ "$TAG" = "latest" ]; then
  TAG=$(gh release view --repo "$REPO" --json tagName --jq .tagName) \
    || die "no releases found in $REPO"
fi

archive="leadkit_${TAG}_${os}_${arch}.tar.gz"
tmp=$(mktemp -d)
tmpbin=""
cleanup() {
  rm -rf "$tmp"
  [ -z "$tmpbin" ] || rm -f "$tmpbin"
}
# INT and TERM must EXIT, not merely clean up: a trap handler that returns lets the script
# carry on running after the signal. exit fires the EXIT trap, so cleanup runs once there.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

printf 'leadkit %s (%s/%s)\n' "$TAG" "$os" "$arch"
gh release download "$TAG" --repo "$REPO" --pattern "$archive" --pattern SHA256SUMS --dir "$tmp" \
  || die "could not download $archive from $TAG"

# Verify before trusting. This detects a corrupted or truncated download. It does NOT establish
# provenance: SHA256SUMS arrives from the same release over the same channel, so whoever can
# replace the archive can replace the sums with it. Nothing here is signed.
[ -f "$tmp/SHA256SUMS" ] || die "release has no SHA256SUMS; refusing to install unverified"

# Extract the line FIRST and require it, rather than piping grep into the checker. This is
# /bin/sh with no pipefail, so a pipeline's status is the LAST command's and an empty match
# would pass. macOS ships a /sbin/sha256sum that exits 0 on empty input.
line=$(grep " $archive\$" "$tmp/SHA256SUMS") \
  || die "SHA256SUMS has no entry for $archive — refusing to install unverified"
[ -n "$line" ] || die "SHA256SUMS entry for $archive is empty — refusing to install unverified"

( cd "$tmp" && if command -v sha256sum >/dev/null 2>&1; then
    printf '%s\n' "$line" | sha256sum -c - >/dev/null
  else
    printf '%s\n' "$line" | shasum -a 256 -c - >/dev/null
  fi ) || die "checksum mismatch for $archive — do not use this download"
printf 'checksum ok\n'

tar -xzf "$tmp/$archive" -C "$tmp"
src="$tmp/leadkit_${TAG}_${os}_${arch}/leadkit"
[ -x "$src" ] || die "archive did not contain an executable at the expected path"

# Atomic replacement, never an in-place copy. Writing over a running, mmap'd binary
# corrupts the image of the process already executing it. Copy alongside, then rename:
# rename is atomic on the same filesystem, so a concurrent run either sees the old inode
# or the new one and never a half-written file.
mkdir -p "$(dirname "$BIN")"
tmpbin="$BIN.tmp.$$"
cp "$src" "$tmpbin"
chmod +x "$tmpbin"
mv -f "$tmpbin" "$BIN"
tmpbin="" # renamed into place; nothing left for cleanup to remove

printf 'installed: %s\n' "$BIN"
"$BIN" version

# `leadkit` resolves only if something on PATH is named `leadkit`; the default target is a
# file named leadkit-bin, so having its directory on PATH does not help — only a symlink does.
found=$(command -v leadkit 2>/dev/null || true)
if [ "$found" = "$BIN" ] || { [ -n "$found" ] && [ "$(readlink "$found" 2>/dev/null)" = "$BIN" ]; }; then
  printf '\nleadkit on PATH resolves to %s\n' "$BIN"
elif [ -n "$found" ]; then
  printf '\nWARNING: leadkit on PATH is %s, not the binary just installed (%s).\nPut the new one ahead of it on PATH, or replace that file deliberately.\n' \
    "$found" "$BIN"
else
  printf '\nleadkit is not on PATH (installed as %s). Symlink it onto PATH:\n  ln -sf %s /usr/local/bin/leadkit\n' \
    "$BIN" "$BIN"
fi
printf '\nNext: leadkit doctor\n'
