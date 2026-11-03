#!/bin/sh
set -e

REPO="kunchenguid/runner-up"

# Prefer ~/.local/bin if it exists and is in PATH (no sudo needed).
# Fall back to /usr/local/bin otherwise.
if echo "$PATH" | tr ':' '\n' | grep -qx "$HOME/.local/bin"; then
  INSTALL_DIR="$HOME/.local/bin"
else
  INSTALL_DIR="/usr/local/bin"
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$ARCH" in
  x86_64|amd64) ARCH="amd64" ;;
  arm64|aarch64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

case "$OS" in
  darwin|linux) ;;
  *) echo "Unsupported OS: $OS"; exit 1 ;;
esac

# Resolve the latest tag without the unauthenticated REST API's rate limit.
latest_url="$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/${REPO}/releases/latest")"
VERSION=""
case "$latest_url" in
  */releases/tag/*)
    VERSION="${latest_url##*/releases/tag/}"
    VERSION="${VERSION%%\?*}"
    VERSION="${VERSION%%#*}"
    VERSION="${VERSION%/}"
    ;;
esac
if [ -z "$VERSION" ]; then
  echo "Failed to determine latest version"
  exit 1
fi

VERSION_NUM="${VERSION#v}"
FILENAME="runner-up-v${VERSION_NUM}-${OS}-${ARCH}.tar.gz"
URL="https://github.com/${REPO}/releases/download/${VERSION}/${FILENAME}"

TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading runner-up ${VERSION} for ${OS}/${ARCH}..."
curl -fsSL "$URL" -o "${TMPDIR}/${FILENAME}"
curl -fsSL "https://github.com/${REPO}/releases/download/${VERSION}/checksums.txt" -o "${TMPDIR}/checksums.txt"

# Install only the archive the release's checksums.txt lists, byte for byte.
EXPECTED="$(awk -v name="$FILENAME" '$2 == name || $2 == "*" name { print $1; exit }' "${TMPDIR}/checksums.txt")"
if [ -z "$EXPECTED" ]; then
  echo "checksums.txt does not list ${FILENAME}; not installing"
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL="$(sha256sum "${TMPDIR}/${FILENAME}" | awk '{ print $1 }')"
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL="$(shasum -a 256 "${TMPDIR}/${FILENAME}" | awk '{ print $1 }')"
else
  echo "Need sha256sum or shasum to check the download; not installing"
  exit 1
fi
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "${FILENAME} does not match checksums.txt (got ${ACTUAL}, want ${EXPECTED}); not installing"
  exit 1
fi

tar xzf "${TMPDIR}/${FILENAME}" -C "$TMPDIR"

if [ -w "$INSTALL_DIR" ]; then
  mkdir -p "$INSTALL_DIR"
  mv "${TMPDIR}/runner-up" "${INSTALL_DIR}/runner-up"
else
  echo "Installing to ${INSTALL_DIR} (requires sudo)..."
  sudo mkdir -p "$INSTALL_DIR"
  sudo mv "${TMPDIR}/runner-up" "${INSTALL_DIR}/runner-up"
fi

echo "runner-up ${VERSION} installed to ${INSTALL_DIR}/runner-up"
