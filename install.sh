#!/usr/bin/env bash
set -euo pipefail

REPO="jpgomesf/mcp-postgres"
BINARY="mcp-postgres"
INSTALL_DIR="/usr/local/bin"

# Detect OS and architecture
OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64) ARCH="arm64" ;;
  arm64)   ARCH="arm64" ;;
  *)       echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

# Validate supported platform
if [[ "$OS" == "darwin" && "$ARCH" != "arm64" ]]; then
  echo "Unsupported: macOS requires arm64 (Apple Silicon)"
  exit 1
fi
if [[ "$OS" == "linux" && "$ARCH" != "amd64" ]]; then
  echo "Unsupported: Linux requires amd64 (x86_64)"
  exit 1
fi

# Get latest version
echo "Fetching latest release..."
VERSION="$(gh release view --repo "$REPO" --json tagName -q .tagName)"
echo "Latest version: $VERSION"

# Download
ASSET="${BINARY}_${VERSION#v}_${OS}_${ARCH}.tar.gz"
CHECKSUM_FILE="checksums.txt"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

echo "Downloading $ASSET..."
gh release download "$VERSION" --repo "$REPO" --pattern "$ASSET" --dir "$TMPDIR"
gh release download "$VERSION" --repo "$REPO" --pattern "$CHECKSUM_FILE" --dir "$TMPDIR"

# Verify checksum
echo "Verifying checksum..."
cd "$TMPDIR"
if command -v sha256sum &>/dev/null; then
  sha256sum --check --ignore-missing "$CHECKSUM_FILE"
elif command -v shasum &>/dev/null; then
  shasum -a 256 --check --ignore-missing "$CHECKSUM_FILE"
else
  echo "Warning: no sha256sum or shasum found, skipping verification"
fi

# Extract and install
tar -xzf "$ASSET"
echo "Installing to $INSTALL_DIR/$BINARY (may require sudo)..."
if [[ -w "$INSTALL_DIR" ]]; then
  mv "$BINARY" "$INSTALL_DIR/$BINARY"
else
  sudo mv "$BINARY" "$INSTALL_DIR/$BINARY"
fi

echo "Installed $BINARY $VERSION to $INSTALL_DIR/$BINARY"
