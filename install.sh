#!/usr/bin/env bash
# install.sh -- One-line installer for nodex-supervisor (nodexa-agent)
# Installs directly to /usr/local/bin so it is immediately available on PATH.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.sh | sh
#
# Or with options:
#   ./install.sh [--dir <install_dir>] [--version <tag_or_version>]

set -e

REPO="adhuldas/nodex-supervisor"
INSTALL_DIR="/usr/local/bin"
BIN_NAME="nodex-supervisor"
ALIAS_NAME="nodexa-agent"
TARGET_VERSION=""

while [ $# -gt 0 ]; do
    case "$1" in
        --dir)
            INSTALL_DIR="$2"
            shift 2
            ;;
        --version)
            TARGET_VERSION="$2"
            shift 2
            ;;
        --uninstall|-u)
            DO_UNINSTALL=1
            shift
            ;;
        -h|--help)
            echo "Usage: $0 [--dir <path>] [--version <version>] [--uninstall]"
            exit 0
            ;;
        *)
            echo "Unknown argument: $1"
            exit 1
            ;;
    esac
done

if [ "${DO_UNINSTALL}" = "1" ]; then
    echo "==> Uninstalling ${BIN_NAME} and ${ALIAS_NAME} from ${INSTALL_DIR}..."
    removed=0
    for target_bin in "${INSTALL_DIR}/${BIN_NAME}" "${INSTALL_DIR}/${ALIAS_NAME}"; do
        if [ -e "${target_bin}" ] || [ -L "${target_bin}" ]; then
            if [ -w "${INSTALL_DIR}" ]; then
                rm -f "${target_bin}"
            else
                echo "  Requesting sudo permissions to remove ${target_bin}..."
                sudo rm -f "${target_bin}"
            fi
            echo "  Removed ${target_bin}"
            removed=1
        fi
    done

    if [ ${removed} -eq 1 ]; then
        echo "==> Successfully uninstalled ${BIN_NAME}."
    else
        echo "==> No existing installation found in ${INSTALL_DIR}."
    fi
    exit 0
fi

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
RAW_ARCH="$(uname -m)"

case "$RAW_ARCH" in
    x86_64|amd64) ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    armv7l|armv7|armhf) ARCH="armv7" ;;
    *) echo "Unsupported architecture: $RAW_ARCH" && exit 1 ;;
esac

echo "==> Installing ${BIN_NAME} for ${OS}-${ARCH}..."

# Helper for privileged operations
run_install() {
    local src="$1"
    local dest="$2"

    mkdir -p "${INSTALL_DIR}" 2>/dev/null || sudo mkdir -p "${INSTALL_DIR}"

    if [ -w "${INSTALL_DIR}" ]; then
        cp -f "${src}" "${dest}"
        chmod 0755 "${dest}"
        ln -sf "${dest}" "${INSTALL_DIR}/${ALIAS_NAME}"
    else
        echo "  Requesting sudo permissions to install to ${INSTALL_DIR}..."
        sudo cp -f "${src}" "${dest}"
        sudo chmod 0755 "${dest}"
        sudo ln -sf "${dest}" "${INSTALL_DIR}/${ALIAS_NAME}"
    fi
}

# 1. If local Go repository source is detected, build and install locally
if [ -f "./cmd/nodexa-agent/main.go" ] && command -v go >/dev/null 2>&1; then
    echo "  Building from local source..."
    VER="0.3.7"
    if [ -f "./AGENT_VERSION" ]; then
        VER="$(tr -d '[:space:]' < ./AGENT_VERSION)"
    fi
    COMMIT="$(git rev-parse --short=12 HEAD 2>/dev/null || echo "local")"
    DATE="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

    LDFLAGS="-s -w \
      -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.AgentVersion=${VER} \
      -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.OSVersion=${VER} \
      -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.Commit=${COMMIT} \
      -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.BuildDate=${DATE}"

    TMP_BIN="$(mktemp -t "${BIN_NAME}.XXXXXX")"
    CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" -o "${TMP_BIN}" ./cmd/nodexa-agent

    TARGET="${INSTALL_DIR}/${BIN_NAME}"
    run_install "${TMP_BIN}" "${TARGET}"
    rm -f "${TMP_BIN}"

    echo "==> Successfully installed ${BIN_NAME} to ${TARGET}"
    echo "==> Alias ${ALIAS_NAME} linked to ${TARGET}"
    "${TARGET}" version || "${TARGET}" --version || true
    exit 0
fi

# 2. Otherwise, query GitHub Releases
if [ -n "${TARGET_VERSION}" ]; then
    case "${TARGET_VERSION}" in
        v*) TAG="${TARGET_VERSION}" ;;
        *) TAG="v${TARGET_VERSION}" ;;
    esac
    API_URL="https://api.github.com/repos/${REPO}/releases/tags/${TAG}"
else
    API_URL="https://api.github.com/repos/${REPO}/releases/latest"
fi

RELEASE_JSON="$(curl -fsSL "${API_URL}" 2>/dev/null || true)"
TAG_NAME="$(echo "${RELEASE_JSON}" | grep '"tag_name":' | head -n 1 | sed -E 's/.*"([^"]+)".*/\1/' || true)"

if [ -z "${TAG_NAME}" ]; then
    # Fallback to `go install` if go toolchain exists
    if command -v go >/dev/null 2>&1; then
        echo "  No GitHub release found. Building with go install..."
        go install "github.com/${REPO}/cmd/nodexa-agent@latest"
        GOPATH_BIN="$(go env GOPATH)/bin/nodexa-agent"
        if [ -f "${GOPATH_BIN}" ]; then
            TARGET="${INSTALL_DIR}/${BIN_NAME}"
            run_install "${GOPATH_BIN}" "${TARGET}"
            echo "==> Successfully installed ${BIN_NAME} to ${TARGET}"
            "${TARGET}" version || true
            exit 0
        fi
    fi
    echo "Error: Could not find release on GitHub (${REPO}) and local Go toolchain is not available."
    exit 1
fi

echo "  Found release ${TAG_NAME}"

# Determine asset URL
ASSET_NAME="nodex-supervisor_${OS}_${ARCH}.tar.gz"
DOWNLOAD_URL="$(echo "${RELEASE_JSON}" | grep "browser_download_url.*${ASSET_NAME}" | head -n 1 | cut -d '"' -f 4 || true)"

if [ -z "${DOWNLOAD_URL}" ]; then
    DOWNLOAD_URL="https://github.com/${REPO}/releases/download/${TAG_NAME}/${ASSET_NAME}"
fi

echo "  Downloading ${DOWNLOAD_URL}..."
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

if ! curl -fsSL "${DOWNLOAD_URL}" | tar -xz -C "${TMP_DIR}"; then
    echo "Error: Failed to download or unpack ${DOWNLOAD_URL}"
    exit 1
fi

EXTRACTED_BIN=""
if [ -f "${TMP_DIR}/${BIN_NAME}" ]; then
    EXTRACTED_BIN="${TMP_DIR}/${BIN_NAME}"
elif [ -f "${TMP_DIR}/${ALIAS_NAME}" ]; then
    EXTRACTED_BIN="${TMP_DIR}/${ALIAS_NAME}"
fi

if [ -z "${EXTRACTED_BIN}" ]; then
    echo "Error: Could not find binary in release archive."
    exit 1
fi

TARGET="${INSTALL_DIR}/${BIN_NAME}"
run_install "${EXTRACTED_BIN}" "${TARGET}"

echo "==> Successfully installed ${BIN_NAME} to ${TARGET}"
echo "==> Alias ${ALIAS_NAME} linked to ${TARGET}"
"${TARGET}" version || "${TARGET}" --version || true
