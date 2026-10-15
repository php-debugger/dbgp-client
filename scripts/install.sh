#!/bin/sh
# dbgp installer (Linux/macOS). Installs the latest release system-wide.
#
# Usage:
#   curl -fsSL https://github.com/php-debugger/dbgp-client/releases/latest/download/install.sh | sh
#   wget -qO- https://github.com/php-debugger/dbgp-client/releases/latest/download/install.sh | sh
#
# Environment overrides:
#   INSTALL_DIR=/path   where to place the binary (default: /usr/local/bin)
#   VERSION=0.1.0       the release to install (default: the latest)
#
# sudo is used only when INSTALL_DIR is not writable.
#
# Because the archive is fetched with curl/wget (not a browser), the binary is
# not quarantined, so macOS Gatekeeper does not block it. As a safety net the
# script also clears the quarantine attribute if present.
set -eu

REPO="php-debugger/dbgp-client"
BIN="dbgp"
INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
VERSION="${VERSION:-}"

info() { printf '%s\n' "$*"; }
fail() { printf 'error: %s\n' "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

download() { # download URL FILE
	if have curl; then curl -fsSL "$1" -o "$2"
	elif have wget; then wget -q "$1" -O "$2"
	else fail "need curl or wget to download"; fi
}

# --- detect OS ---
kernel="$(uname -s)"
case "$kernel" in
	Linux) OS="linux" ;;
	Darwin) OS="macos" ;;
	*) fail "unsupported operating system: $kernel (install with: go install github.com/$REPO/cmd/dbgp@latest)" ;;
esac

# --- detect architecture ---
machine="$(uname -m)"
case "$machine" in
	x86_64 | amd64) ARCH="amd64" ;;
	arm64 | aarch64) ARCH="arm64" ;;
	*) fail "unsupported architecture: $machine (install with: go install github.com/$REPO/cmd/dbgp@latest)" ;;
esac

ASSET="dbgp-${OS}-${ARCH}.tar.gz"
if [ -n "$VERSION" ]; then
	URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
	info "Installing $BIN $VERSION ($OS/$ARCH)"
else
	# The releases/latest/download URL always resolves to the newest release.
	URL="https://github.com/$REPO/releases/latest/download/$ASSET"
	info "Installing $BIN (latest, $OS/$ARCH)"
fi

# --- download + extract ---
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM
download "$URL" "$tmp/$ASSET" || fail "failed to download $URL"
tar -xzf "$tmp/$ASSET" -C "$tmp" || fail "failed to extract $ASSET"
[ -f "$tmp/$BIN" ] || fail "archive did not contain $BIN"
chmod +x "$tmp/$BIN"
# Clear any quarantine attribute so macOS Gatekeeper does not block it.
if [ "$OS" = "macos" ]; then
	xattr -d com.apple.quarantine "$tmp/$BIN" 2>/dev/null || true
fi

# --- install ---
# sudo only if the directory, or the one it would be created in, is not writable.
target="$INSTALL_DIR"
while [ ! -d "$target" ]; do target="$(dirname "$target")"; done
SUDO=""
if [ ! -w "$target" ]; then
	have sudo || fail "cannot write to $INSTALL_DIR: run as root, or set INSTALL_DIR"
	info "Installing to $INSTALL_DIR needs administrator rights: using sudo"
	SUDO="sudo"
fi
$SUDO mkdir -p "$INSTALL_DIR" || fail "cannot create $INSTALL_DIR"
# mv replaces the file rather than writing into it, so a running dbgp (such
# as the background MCP server) keeps running the old one.
$SUDO mv -f "$tmp/$BIN" "$INSTALL_DIR/$BIN" || fail "cannot write to $INSTALL_DIR"

info ""
info "Installed $("$INSTALL_DIR/$BIN" -version) to $INSTALL_DIR/$BIN"
case ":$PATH:" in
	*":$INSTALL_DIR:"*) ;;
	*)
		info "Add $INSTALL_DIR to your PATH, e.g. in your shell's profile:"
		info "    export PATH=\"$INSTALL_DIR:\$PATH\""
		;;
esac
if "$INSTALL_DIR/$BIN" mcp status >/dev/null 2>&1; then
	info "The background MCP server still runs the previous version: restart it with"
	info "    dbgp mcp stop && dbgp mcp start"
fi
