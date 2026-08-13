#!/usr/bin/env bash
#
# Install sal.
#
#   curl -fsSL https://raw.githubusercontent.com/lpezet/secure-agent-lab-cli/main/install.sh | bash
#   curl -fsSL https://raw.githubusercontent.com/lpezet/secure-agent-lab-cli/main/install.sh | bash -s v0.1.0
#
# This is kept short on purpose. Piping a script into a shell to install a
# security tool is a reasonable thing to object to, so the mitigation is that
# you can read the whole thing in a minute, and that cloning the repo and
# running `make build` is a supported path rather than a footnote.
#
# The checksum is verified BEFORE anything is extracted. If cosign is on PATH
# the signature over that checksum file is verified too; if it is not, the
# script says so rather than quietly skipping it.
set -euo pipefail

REPO="lpezet/secure-agent-lab-cli"
VERSION="${1:-latest}"
INSTALL_DIR="${SAL_INSTALL_DIR:-$HOME/.local/bin}"

die() { printf 'install: %s\n' "$*" >&2; exit 1; }
note() { printf '%s\n' "$*" >&2; }

for tool in curl tar mktemp; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

# sha256sum on Linux, shasum on macOS. One of them must exist: an install that
# skipped verification because the tool was missing would be worse than none.
if command -v sha256sum >/dev/null 2>&1; then
  sha_check() { sha256sum --check --ignore-missing --status "$1"; }
elif command -v shasum >/dev/null 2>&1; then
  sha_check() { shasum -a 256 --check --ignore-missing --status "$1"; }
else
  die "neither sha256sum nor shasum found, so the download cannot be verified"
fi

case "$(uname -s)" in
  Linux)  os=linux ;;
  Darwin) os=darwin ;;
  *)      die "unsupported OS $(uname -s); sal drives docker compose from the host, and on Windows the supported path is WSL" ;;
esac

case "$(uname -m)" in
  x86_64|amd64)  arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *)             die "unsupported architecture $(uname -m)" ;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" \
    | grep '"tag_name":' | head -1 | sed -E 's/.*"([^"]+)".*/\1/')
  [ -n "$VERSION" ] || die "could not determine the latest release"
fi

archive="sal_${VERSION#v}_${os}_${arch}.tar.gz"
base="https://github.com/${REPO}/releases/download/${VERSION}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

note "downloading sal ${VERSION} for ${os}/${arch}"
curl -fsSL -o "${tmp}/${archive}" "${base}/${archive}" \
  || die "no such release asset: ${archive}"
curl -fsSL -o "${tmp}/checksums.txt" "${base}/checksums.txt" \
  || die "release ${VERSION} publishes no checksums.txt; refusing to install unverified"

if command -v cosign >/dev/null 2>&1; then
  curl -fsSL -o "${tmp}/checksums.txt.sig" "${base}/checksums.txt.sig"
  curl -fsSL -o "${tmp}/checksums.txt.pem" "${base}/checksums.txt.pem"
  cosign verify-blob "${tmp}/checksums.txt" \
    --signature "${tmp}/checksums.txt.sig" \
    --certificate "${tmp}/checksums.txt.pem" \
    --certificate-identity-regexp "https://github.com/${REPO}/.*" \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com \
    >/dev/null 2>&1 || die "signature over checksums.txt did not verify"
  note "signature verified"
else
  note "cosign not found — checksum verified, signature NOT checked."
  note "  to check it: https://github.com/${REPO}/releases/tag/${VERSION}"
fi

( cd "$tmp" && sha_check checksums.txt ) || die "checksum mismatch for ${archive}"
note "checksum verified"

tar -xzf "${tmp}/${archive}" -C "$tmp" sal
mkdir -p "$INSTALL_DIR"
mv "${tmp}/sal" "${INSTALL_DIR}/sal"
chmod 0755 "${INSTALL_DIR}/sal"

note "installed ${INSTALL_DIR}/sal"
case ":${PATH}:" in
  *":${INSTALL_DIR}:"*) ;;
  *) note "note: ${INSTALL_DIR} is not on your PATH" ;;
esac

# Deliberately not run from inside a lab: sal is a host-side tool, and putting
# it on PATH in the lab container would hand the agent an interface for
# widening its own allowlist.
"${INSTALL_DIR}/sal" --version
