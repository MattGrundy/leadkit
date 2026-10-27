#!/bin/sh
# Tests for install.sh. No network, no real gh: a stub `gh` on PATH serves a fixture release,
# so every case asserts the two things that matter for an installer — the exit status AND
# whether the destination file exists. (A "checksum ok" that still installs is the bug this
# harness exists to catch; asserting on output text alone would not see it.)
#
#   sh scripts/test-install.sh
#   SH_UNDER_TEST=dash sh scripts/test-install.sh   # CI's /bin/sh is dash, macOS's is bash
set -u

sh_under_test="${SH_UNDER_TEST:-sh}"

here=$(cd "$(dirname "$0")/.." && pwd)
installer="$here/install.sh"
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT INT TERM
fails=0
ran=0

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$(uname -m)" in
  arm64|aarch64) arch=arm64 ;;
  *) arch=amd64 ;;
esac
tag=v9.9.9
archive="leadkit_${tag}_${os}_${arch}.tar.gz"

hash256() { if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1"; else shasum -a 256 "$1"; fi; }

fix="$root/fixture"
mkdir -p "$fix/pkg/leadkit_${tag}_${os}_${arch}"
printf '#!/bin/sh\necho "leadkit %s"\n' "$tag" > "$fix/pkg/leadkit_${tag}_${os}_${arch}/leadkit"
chmod +x "$fix/pkg/leadkit_${tag}_${os}_${arch}/leadkit"
tar -C "$fix/pkg" -czf "$fix/$archive" "leadkit_${tag}_${os}_${arch}"
good_sum=$(cd "$fix" && hash256 "$archive" | awk '{print $1}')

stubs="$root/stubs"
mkdir -p "$stubs"
cat > "$stubs/gh" <<'STUB'
#!/bin/sh
case "$1 $2" in
  "auth status") exit 0 ;;
  "release view") echo "v9.9.9"; exit 0 ;;
  "release download")
    # STUB_TERM_PARENT: deliver SIGTERM to the installer mid-download, as a user's kill would.
    [ -z "${STUB_TERM_PARENT:-}" ] || { kill -TERM "$PPID"; sleep 1; }
    dir=""; pats=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --dir) dir="$2"; shift ;;
        --pattern) pats="$pats $2"; shift ;;
      esac
      shift
    done
    for p in $pats; do [ -f "$FIXTURE_DIR/$p" ] && cp "$FIXTURE_DIR/$p" "$dir/$p"; done
    exit 0 ;;
esac
echo "stub gh: unexpected: $*" >&2; exit 99
STUB
chmod +x "$stubs/gh"

# Stand-in for macOS's sha256sum, which exits 0 on EMPTY input; reproduces it on any host.
lenient="$root/lenient"
mkdir -p "$lenient"
cat > "$lenient/sha256sum" <<'STUB'
#!/bin/sh
in=$(cat)
[ -n "$in" ] || exit 0
printf '%s\n' "$in" | shasum -a 256 "$@"
STUB
chmod +x "$lenient/sha256sum"

# A chmod that fails on the installer's temp file, to exercise the failure path AFTER the
# temp file exists.
failchmod="$root/failchmod"
mkdir -p "$failchmod"
cat > "$failchmod/chmod" <<'STUB'
#!/bin/sh
case "$*" in *.tmp.*) exit 1 ;; esac
exec /bin/chmod "$@"
STUB
chmod +x "$failchmod/chmod"

# new_case NAME: a fresh sandbox. HOME and the install target live inside it.
new_case() {
  ran=$((ran + 1))
  name="$1"
  cdir="$root/case$ran"
  rm -rf "$cdir"; mkdir -p "$cdir/fixture" "$cdir/home"
  cp "$fix/$archive" "$cdir/fixture/$archive"
  bin="$cdir/home/bin/leadkit-bin"
}

# run_install [EXTRA_PATH]: runs the installer in the sandbox; sets $rc and $out.
run_install() {
  out=$(HOME="$cdir/home" LEADKIT_BIN="$bin" FIXTURE_DIR="$cdir/fixture" PATH="${1:+$1:}$stubs:$PATH" "$sh_under_test" "$installer" "$tag" 2>&1)
  rc=$?
}

fail() { printf 'FAIL  %s: %s\n' "$name" "$1"; printf '%s\n' "$out" | sed 's/^/        | /'; fails=$((fails + 1)); }
pass() { printf 'ok    %s\n' "$name"; }

expect_refused() {
  if [ "$rc" -eq 0 ]; then fail "exited 0, wanted a refusal"
  elif [ -e "$bin" ]; then fail "refused, but the destination exists"
  elif ls "$(dirname "$bin")"/*.tmp.* >/dev/null 2>&1; then fail "left a stray temp file next to the destination"
  else pass; fi
}

new_case "good checksum installs"
printf '%s  %s\n' "$good_sum" "$archive" > "$cdir/fixture/SHA256SUMS"
run_install
if [ "$rc" -ne 0 ]; then fail "exit $rc"
elif [ ! -x "$bin" ]; then fail "no executable at the destination"
elif [ "$("$bin" version)" != "leadkit $tag" ]; then fail "installed binary reports the wrong version"
else pass; fi

for variant in native lenient-sha256sum; do
  new_case "no line for the archive is refused ($variant)"
  printf '%s  %s\n' "$good_sum" "leadkit_${tag}_plan9_mips.tar.gz" > "$cdir/fixture/SHA256SUMS"
  extra=""; [ "$variant" = lenient-sha256sum ] && extra="$lenient"
  run_install "$extra"
  expect_refused
done

new_case "wrong hash is refused"
printf '%064d  %s\n' 0 "$archive" > "$cdir/fixture/SHA256SUMS"
run_install
expect_refused

new_case "missing SHA256SUMS is refused"
run_install
expect_refused

new_case "empty SHA256SUMS is refused"
: > "$cdir/fixture/SHA256SUMS"
run_install "$lenient"
expect_refused

new_case "destination that is a directory is refused"
printf '%s  %s\n' "$good_sum" "$archive" > "$cdir/fixture/SHA256SUMS"
mkdir -p "$bin"
run_install
if [ "$rc" -eq 0 ]; then fail "exited 0, wanted a refusal"
elif ls "$bin"/* >/dev/null 2>&1; then fail "moved a file into the directory"
else pass; fi

# PATH advice must be true. The default target is a FILE named leadkit-bin, so its directory
#    being on PATH does not make `leadkit` resolve; the advice must say to symlink.
new_case "leadkit-bin on PATH still gets the symlink advice"
printf '%s  %s\n' "$good_sum" "$archive" > "$cdir/fixture/SHA256SUMS"
run_install "$cdir/home/bin"
if [ "$rc" -ne 0 ]; then fail "exit $rc"
elif ! printf '%s' "$out" | grep -q 'ln -s'; then fail "no symlink advice, though 'leadkit' does not resolve"
else pass; fi

new_case "a binary named leadkit on PATH is reported as resolving"
bin="$cdir/home/bin/leadkit"
printf '%s  %s\n' "$good_sum" "$archive" > "$cdir/fixture/SHA256SUMS"
run_install "$cdir/home/bin"
if [ "$rc" -ne 0 ]; then fail "exit $rc"
elif printf '%s' "$out" | grep -q 'ln -s'; then fail "advised a symlink though leadkit already resolves"
elif ! printf '%s' "$out" | grep -q 'on PATH'; then fail "said nothing about PATH"
else pass; fi

new_case "a failed install leaves no temp file behind"
printf '%s  %s\n' "$good_sum" "$archive" > "$cdir/fixture/SHA256SUMS"
run_install "$failchmod"
expect_refused

new_case "SIGTERM stops the install and removes the scratch dir"
printf '%s  %s\n' "$good_sum" "$archive" > "$cdir/fixture/SHA256SUMS"
mkdir -p "$cdir/tmp"
out=$(HOME="$cdir/home" LEADKIT_BIN="$bin" FIXTURE_DIR="$cdir/fixture" TMPDIR="$cdir/tmp" STUB_TERM_PARENT=1 PATH="$stubs:$PATH" "$sh_under_test" "$installer" "$tag" 2>&1)
rc=$?
if [ "$rc" -ne 143 ]; then fail "exit $rc after SIGTERM, wanted 143 (the script must stop at the signal, not limp on to fail later)"
elif [ -e "$bin" ]; then fail "carried on and installed after SIGTERM"
elif [ -n "$(ls -A "$cdir/tmp")" ]; then fail "left its scratch directory behind"
else pass; fi

printf '\n%d case(s), %d failure(s)\n' "$ran" "$fails"
[ "$fails" -eq 0 ]
