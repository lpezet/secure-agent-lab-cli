#!/usr/bin/env bash
# Tests install.sh — the one part of sal that is not Go, and the part a user
# meets first.
#
# `curl … | bash` as the install path for a security tool is a look, and the
# mitigations are the checksum, the signature over it and a script short enough
# to read. All three are only worth anything if they actually REFUSE, so most of
# what is below is the refusals: a tampered archive, a release with no
# checksums, a signature that does not verify.
#
# No network and no containers. `curl` and `cosign` are fakes first on PATH, the
# same technique the txtar scripts use for `docker` — it exercises the real
# script rather than a rewritten copy of it, and the refusals become things a
# test can actually produce.
#
# The tier that still needs containers is the one this cannot do: busybox tar,
# shasum instead of sha256sum, a PATH with no writable directory. See CLAUDE.md.
#
# Exit codes: 0 pass · 1 a test failed · 2 cannot run.
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
script="$root/install.sh"

[ -f "$script" ] || { printf 'cannot find install.sh at %s\n' "$script" >&2; exit 2; }
for tool in tar mktemp; do
  command -v "$tool" >/dev/null 2>&1 || { printf '%s is required\n' "$tool" >&2; exit 2; }
done
if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
  printf 'neither sha256sum nor shasum found\n' >&2
  exit 2
fi

# Named with the PID and removed on exit, so a run never collides with another
# and never leaves anything behind.
work=$(mktemp -d "${TMPDIR:-/tmp}/sal-install-test-$$-XXXXXX") || exit 2
trap 'rm -rf "$work"' EXIT

passed=0 failed=0
ok()   { passed=$((passed + 1)); printf '  ok      %s\n' "$1"; }
bad()  { failed=$((failed + 1)); printf '  FAILED  %s\n' "$1"; }
check() { # check <description> <condition-as-exit-status>
  if [ "$2" -eq 0 ]; then ok "$1"; else bad "$1"; fi
}

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1
  fi
}

# ---------------------------------------------------------------- a release
#
# Built rather than downloaded: the archive layout and the checksum file are
# what install.sh consumes, so producing them here is what makes the checks
# meaningful.
release="$work/release"
mkdir -p "$release"

version=v1.2.3
case "$(uname -s)" in Linux) os=linux ;; Darwin) os=darwin ;; *) printf 'unsupported test OS\n' >&2; exit 2 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) printf 'unsupported test arch\n' >&2; exit 2 ;; esac
archive="sal_${version#v}_${os}_${arch}.tar.gz"

staging="$work/staging"
mkdir -p "$staging"
cat > "$staging/sal" <<'STUB'
#!/bin/sh
# Stands in for the real binary: the script's last act is to run `sal --version`,
# and what matters is that it ran the file it just installed.
[ "$1" = "--version" ] && echo "sal 1.2.3 (test stub)"
STUB
chmod 0755 "$staging/sal"
tar -czf "$release/$archive" -C "$staging" sal
( cd "$release" && printf '%s  %s\n' "$(sha256_of "$archive")" "$archive" > checksums.txt )
printf 'a bundle\n' > "$release/checksums.txt.bundle"

# ------------------------------------------------------------------- fakes
bin="$work/bin"
mkdir -p "$bin"

cat > "$bin/curl" <<'FAKE'
#!/bin/sh
# Serves FAKE_RELEASE_DIR. Exits 22 for a missing asset, which is what curl -f
# does for a 404 — the difference between "this release has no checksums" and
# "the network is down" is one install.sh has to get right.
out=""
url=""
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift 2 ;;
	-*) shift ;;
	*) url=$1; shift ;;
	esac
done
case "$url" in
*api.github.com*)
	printf '{"tag_name": "%s"}\n' "${FAKE_LATEST:-v1.2.3}"
	exit 0
	;;
esac
src="$FAKE_RELEASE_DIR/${url##*/}"
[ -f "$src" ] || exit 22
if [ -n "$out" ]; then cp "$src" "$out"; else cat "$src"; fi
FAKE

# In its own directory, and only put on PATH by the tests that want it: the
# no-cosign path is a real one — most machines do not have it — and it has to
# say the signature went unchecked rather than being quiet about it.
cosignbin="$work/cosign-bin"
mkdir -p "$cosignbin"
cat > "$cosignbin/cosign" <<'FAKE'
#!/bin/sh
# Only its exit status matters to install.sh. A real signature check cannot be
# reproduced here without an OIDC identity, and what is being tested is what
# the script DOES with a verdict, not the verdict.
exit "${FAKE_COSIGN_STATUS:-0}"
FAKE

cat > "$bin/uname" <<'FAKE'
#!/bin/sh
case "$1" in
-s) echo "${FAKE_UNAME_S:-Linux}" ;;
-m) echo "${FAKE_UNAME_M:-x86_64}" ;;
*) echo unknown ;;
esac
FAKE

chmod 0755 "$bin"/* "$cosignbin"/*

# A PATH the harness controls completely, rather than the developer's.
#
# Not tidiness: whether install.sh takes the cosign branch is decided by
# whether cosign is on PATH, so inheriting one decides which half of the script
# is under test — and a machine with cosign installed would silently never
# exercise the "signature NOT checked" path at all. This was not a hypothetical:
# it is how the first run of this harness passed a test it was not running.
basepath="$bin:/usr/bin:/bin"

# run <install-dir> [args...] — install.sh with the fakes in front, capturing
# both streams. cosign is on PATH only when WITH_COSIGN is set.
run() {
	local dir=$1
	local path="$basepath"
	shift
	[ -n "${WITH_COSIGN:-}" ] && path="$cosignbin:$path"
	SAL_INSTALL_DIR="$dir" \
		FAKE_RELEASE_DIR="$FAKE_RELEASE_DIR" \
		FAKE_LATEST="${FAKE_LATEST:-}" \
		FAKE_COSIGN_STATUS="${FAKE_COSIGN_STATUS:-0}" \
		FAKE_UNAME_S="${FAKE_UNAME_S:-}" FAKE_UNAME_M="${FAKE_UNAME_M:-}" \
		PATH="$path" \
		bash "$script" "$@" >"$work/out" 2>"$work/err"
}

said() { grep -q -- "$1" "$work/out" "$work/err"; }

export FAKE_RELEASE_DIR="$release"

printf '\ninstall.sh\n'

# ------------------------------------------------------------- the happy path
target="$work/happy"
run "$target" "$version"
status=$?
check "installs the binary and exits 0" "$status"
check "the binary is on disk and executable" "$([ -x "$target/sal" ] && echo 0 || echo 1)"
check "verifies the checksum before extracting" "$(said 'checksum verified' && echo 0 || echo 1)"
check "runs what it installed" "$(said 'test stub' && echo 0 || echo 1)"

# Without cosign it says the signature was not checked, rather than being quiet
# about it — a skipped check nobody is told about is the failure this whole
# arrangement exists to avoid.
check "says the signature was NOT checked when cosign is missing" \
	"$(said 'signature NOT checked' && echo 0 || echo 1)"

# ------------------------------------------------------------------- latest
target="$work/latest"
FAKE_LATEST=v1.2.3 run "$target"
check "resolves 'latest' from the releases API" "$([ -x "$target/sal" ] && echo 0 || echo 1)"

# --------------------------------------------------------------- a tampered
# archive. THE check: the checksum file is what stands between a mirror and
# your PATH.
tampered="$work/tampered-release"
cp -r "$release" "$tampered"
printf 'not the archive you checksummed' > "$tampered/$archive"
target="$work/tamper"
FAKE_RELEASE_DIR="$tampered" run "$target" "$version"
status=$?
check "refuses an archive whose checksum does not match" "$([ "$status" -ne 0 ] && echo 0 || echo 1)"
check "installs nothing when the checksum is wrong" "$([ ! -e "$target/sal" ] && echo 0 || echo 1)"
check "says which archive did not match" "$(said 'checksum mismatch' && echo 0 || echo 1)"

# ------------------------------------------- a release that publishes no sums
nosums="$work/nosums-release"
cp -r "$release" "$nosums"
rm "$nosums/checksums.txt"
target="$work/nosums"
FAKE_RELEASE_DIR="$nosums" run "$target" "$version"
status=$?
check "refuses a release with no checksums.txt" "$([ "$status" -ne 0 ] && echo 0 || echo 1)"
check "says it is refusing to install unverified" "$(said 'refusing to install unverified' && echo 0 || echo 1)"

# ------------------------------------------------------- a missing asset
target="$work/missing"
run "$target" v9.9.9
status=$?
check "refuses a version whose asset does not exist" "$([ "$status" -ne 0 ] && echo 0 || echo 1)"
check "names the asset it could not find" "$(said 'no such release asset' && echo 0 || echo 1)"

# ------------------------------------------------------------------ cosign
#
# With cosign present the signature is checked, and a verdict of "no" stops the
# install. Anything else would make the signature decorative.
target="$work/signed"
WITH_COSIGN=1 FAKE_COSIGN_STATUS=0 run "$target" "$version"
check "verifies the signature when cosign is present" "$(said 'signature verified' && echo 0 || echo 1)"
check "installs when the signature verifies" "$([ -x "$target/sal" ] && echo 0 || echo 1)"

target="$work/badsig"
WITH_COSIGN=1 FAKE_COSIGN_STATUS=1 run "$target" "$version"
status=$?
check "refuses when the signature does not verify" "$([ "$status" -ne 0 ] && echo 0 || echo 1)"
check "installs nothing when the signature does not verify" "$([ ! -e "$target/sal" ] && echo 0 || echo 1)"

# The bundle format changed with cosign v3, and an older cosign fails in a way
# that looks like a bad signature. Saying so is the difference between "upgrade
# cosign" and "someone tampered with this release".
check "explains that cosign older than v3 cannot read the bundle" \
	"$(said 'older than v3' && echo 0 || echo 1)"

# A release that signs nothing is refused rather than installed unverified,
# once cosign is available to check it.
nobundle="$work/nobundle-release"
cp -r "$release" "$nobundle"
rm "$nobundle/checksums.txt.bundle"
target="$work/nobundle"
FAKE_RELEASE_DIR="$nobundle" WITH_COSIGN=1 FAKE_COSIGN_STATUS=0 run "$target" "$version"
status=$?
check "refuses a release with no signature bundle" "$([ "$status" -ne 0 ] && echo 0 || echo 1)"

# ------------------------------------------------------------ architectures
target="$work/arch"
FAKE_UNAME_M=riscv64 run "$target" "$version"
status=$?
check "refuses an architecture there is no build for" "$([ "$status" -ne 0 ] && echo 0 || echo 1)"
check "names the architecture it refused" "$(said 'unsupported architecture' && echo 0 || echo 1)"

target="$work/os"
FAKE_UNAME_S=MINGW64_NT run "$target" "$version"
status=$?
check "refuses an OS there is no build for" "$([ "$status" -ne 0 ] && echo 0 || echo 1)"
# Windows is not an oversight: sal drives docker compose and reads a
# loopback-published port on the host, so the supported path is WSL.
check "points Windows at WSL rather than just refusing" "$(said 'WSL' && echo 0 || echo 1)"

# ---------------------------------------------------------------------- PATH
target="$work/notonpath"
run "$target" "$version"
check "says when the install directory is not on PATH" "$(said 'not on your PATH' && echo 0 || echo 1)"

printf '\n%d passed, %d failed\n' "$passed" "$failed"
[ "$failed" -eq 0 ]
