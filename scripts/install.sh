#!/bin/sh
# install.sh — Install docket from GitHub releases
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ALT-F4-LLC/docket/main/scripts/install.sh | sh
#
# Environment variables:
#   DOCKET_INSTALL_DIR  — Installation directory (default: $HOME/.local/bin)
#   DOCKET_VERSION      — Release tag to install (default: nightly)
#
# Examples:
#   # Install latest nightly to default location
#   curl -fsSL https://raw.githubusercontent.com/ALT-F4-LLC/docket/main/scripts/install.sh | sh
#
#   # Install a specific version
#   DOCKET_VERSION=v1.0.0 curl -fsSL https://raw.githubusercontent.com/ALT-F4-LLC/docket/main/scripts/install.sh | sh
#
#   # Install to a custom directory
#   DOCKET_INSTALL_DIR=/usr/local/bin curl -fsSL https://raw.githubusercontent.com/ALT-F4-LLC/docket/main/scripts/install.sh | sh

set -eu

main() {
    INSTALL_DIR="${DOCKET_INSTALL_DIR:-$HOME/.local/bin}"
    VERSION="${DOCKET_VERSION:-nightly}"
    REPO="ALT-F4-LLC/docket"
    BASE_URL="https://github.com/${REPO}/releases/download"

    # Detect OS
    OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$OS" in
        linux|darwin) ;;
        *)
            err "unsupported operating system: $OS"
            exit 1
            ;;
    esac

    # Detect architecture
    ARCH="$(uname -m)"
    case "$ARCH" in
        x86_64) ;;
        aarch64) ;;
        arm64) ARCH="aarch64" ;;
        *)
            err "unsupported architecture: $ARCH"
            exit 1
            ;;
    esac

    ARCHIVE="docket-${ARCH}-${OS}.tar.gz"
    URL="${BASE_URL}/${VERSION}/${ARCHIVE}"

    info "installing docket ${VERSION} (${ARCH}-${OS})"
    info "download url: ${URL}"
    info "install directory: ${INSTALL_DIR}"

    # Find a download command
    if command -v curl >/dev/null 2>&1; then
        DOWNLOADER="curl"
    elif command -v wget >/dev/null 2>&1; then
        DOWNLOADER="wget"
    else
        err "either curl or wget is required to download docket"
        exit 1
    fi

    # Create a temporary directory for the download
    TMPDIR="$(mktemp -d)"
    trap 'rm -rf "$TMPDIR"' EXIT

    # Download the archive
    info "downloading ${ARCHIVE}..."
    if [ "$DOWNLOADER" = "curl" ]; then
        if ! curl -fSL --progress-bar -o "${TMPDIR}/${ARCHIVE}" "$URL"; then
            err "download failed — check that version '${VERSION}' exists"
            exit 1
        fi
    else
        if ! wget -q --show-progress -O "${TMPDIR}/${ARCHIVE}" "$URL"; then
            err "download failed — check that version '${VERSION}' exists"
            exit 1
        fi
    fi

    # Extract the binary
    info "extracting..."
    tar -xzf "${TMPDIR}/${ARCHIVE}" -C "$TMPDIR"

    if [ ! -f "${TMPDIR}/docket" ]; then
        err "archive did not contain expected 'docket' binary"
        exit 1
    fi

    # Install the binary
    mkdir -p "$INSTALL_DIR"
    mv "${TMPDIR}/docket" "${INSTALL_DIR}/docket"
    chmod +x "${INSTALL_DIR}/docket"

    info "installed docket to ${INSTALL_DIR}/docket"

    # Verify installation
    if "${INSTALL_DIR}/docket" --version >/dev/null 2>&1; then
        INSTALLED_VERSION="$("${INSTALL_DIR}/docket" version 2>/dev/null || "${INSTALL_DIR}/docket" --version 2>/dev/null || echo "unknown")"
        info "verified: ${INSTALLED_VERSION}"
    else
        warn "installed binary exists but --version check failed"
    fi

    # PATH guidance
    case ":${PATH}:" in
        *":${INSTALL_DIR}:"*) ;;
        *)
            warn "${INSTALL_DIR} is not in your PATH"
            info "add it by appending this to your shell profile:"
            info "  export PATH=\"${INSTALL_DIR}:\$PATH\""
            ;;
    esac
}

info() {
    printf '[docket] %s\n' "$1" >&2
}

warn() {
    printf '[docket] warning: %s\n' "$1" >&2
}

err() {
    printf '[docket] error: %s\n' "$1" >&2
}

main "$@"
