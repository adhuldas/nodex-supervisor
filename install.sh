#!/usr/bin/env bash
# install.sh -- One-line installer for nodex-supervisor (nodexa-agent)
# Installs directly to /usr/local/bin so it is immediately available on PATH.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.sh | sh
#
# Or with options:
#   ./install.sh [--dir <install_dir>] [--version <tag_or_version>] [--fleet-id <id>] [--uninstall]

set -e

REPO="adhuldas/nodex-supervisor"
INSTALL_DIR="/usr/local/bin"
BIN_NAME="nodex-supervisor"
ALIAS_NAME="nodexa-agent"
TARGET_VERSION=""
FLEET_ID="${FLEET_ID:-}"
DO_UNINSTALL=0

INSTALL_TAILSCALE=""

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
        --fleet-id|-f)
            FLEET_ID="$2"
            shift 2
            ;;
        --install-tailscale|--yes-tailscale)
            INSTALL_TAILSCALE=1
            shift
            ;;
        --skip-tailscale|--no-tailscale)
            INSTALL_TAILSCALE=0
            shift
            ;;
        --uninstall|-u)
            DO_UNINSTALL=1
            shift
            ;;
        -h|--help)
            echo "Usage: $0 [--dir <path>] [--version <version>] [--fleet-id <id>] [--install-tailscale|--skip-tailscale] [--uninstall]"
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

    if [ -f "/etc/nodexa/config.json" ]; then
        if [ -w "/etc/nodexa" ]; then
            rm -f "/etc/nodexa/config.json"
        else
            sudo rm -f "/etc/nodexa/config.json" 2>/dev/null || true
        fi
        echo "  Removed /etc/nodexa/config.json"
        removed=1
    fi

    if [ -f "${INSTALL_DIR}/config.json" ]; then
        rm -f "${INSTALL_DIR}/config.json"
        echo "  Removed ${INSTALL_DIR}/config.json"
        removed=1
    fi

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

check_container_prerequisites() {
    echo "==> Checking container deployment prerequisites..."
    local runtime_found=0

    if command -v docker >/dev/null 2>&1; then
        local docker_ver
        docker_ver="$(docker version --format '{{.Server.Version}}' 2>/dev/null || true)"
        if [ -n "${docker_ver}" ]; then
            echo "  [✓] Docker detected and running (server v${docker_ver})."
            echo "      nodex-supervisor will use Docker for container deployment & management."
            runtime_found=1
        else
            echo "  [!] Docker CLI found, but Docker daemon is not responding."
            echo "      Please start Docker (e.g. 'sudo systemctl start docker' or Docker Desktop)"
            echo "      to enable container deployments."
            runtime_found=1
        fi
    elif command -v nerdctl >/dev/null 2>&1; then
        if nerdctl info >/dev/null 2>&1; then
            echo "  [✓] nerdctl detected and running."
            echo "      nodex-supervisor will use nerdctl for container deployment & management."
            runtime_found=1
        else
            echo "  [!] nerdctl CLI found, but containerd is not responding."
            runtime_found=1
        fi
    elif command -v runc >/dev/null 2>&1; then
        echo "  [✓] runc detected."
        echo "      nodex-supervisor will use runc for native OCI container management."
        runtime_found=1
    fi

    if [ ${runtime_found} -eq 0 ]; then
        echo "  [!] Notice: No container runtime (Docker, nerdctl, or runc) was detected."
        echo "      nodex-supervisor requires a container engine to deploy and manage containers."
        echo "      To install Docker, visit: https://docs.docker.com/engine/install/"
    fi
}

check_tailscale_prerequisite() {
    echo "==> Checking Tailscale networking prerequisite..."
    if command -v tailscale >/dev/null 2>&1; then
        local ts_ver
        ts_ver="$(tailscale version 2>/dev/null | head -n 1 || echo "ready")"
        echo "  [✓] Tailscale is installed (${ts_ver})."
        return 0
    fi

    echo ""
    echo "========================================================================"
    echo "  [!] Tailscale is not installed on this system."
    echo "========================================================================"
    echo "  Tailscale provides secure, zero-trust peer-to-peer networking required"
    echo "  by Nodexa for the following features:"
    echo "    • Remote Web Terminal access from the Nodexa Cloud Console"
    echo "    • Live remote container log streaming & live telemetry"
    echo "    • Remote container command execution & debugging (nodexactl exec / SSH)"
    echo "    • Secure encrypted device-to-cloud tunnel (tailnet mesh)"
    echo "========================================================================"
    echo ""

    local do_install="${INSTALL_TAILSCALE}"
    if [ -z "${do_install}" ]; then
        if [ -t 0 ] || [ -r /dev/tty ]; then
            printf "Would you like to install Tailscale now? [y/N]: "
            if [ -t 0 ]; then
                read -r answer
            else
                read -r answer </dev/tty
            fi
            case "${answer}" in
                y|Y|yes|YES) do_install=1 ;;
                *) do_install=0 ;;
            esac
        else
            do_install=0
        fi
    fi

    if [ "${do_install}" = "1" ]; then
        echo "==> Installing Tailscale..."
        local installed=0
        if [ "${OS}" = "linux" ]; then
            if curl -fsSL https://tailscale.com/install.sh | sh 2>/dev/null; then
                installed=1
            elif command -v sudo >/dev/null 2>&1 && curl -fsSL https://tailscale.com/install.sh | sudo sh; then
                installed=1
            fi
            if command -v systemctl >/dev/null 2>&1; then
                sudo systemctl enable --now tailscaled 2>/dev/null || true
            fi
        elif [ "${OS}" = "darwin" ]; then
            if command -v brew >/dev/null 2>&1; then
                brew install tailscale && installed=1
            else
                echo "  Please install Tailscale for macOS from: https://tailscale.com/download/mac"
            fi
        fi

        if command -v tailscale >/dev/null 2>&1 || [ ${installed} -eq 1 ]; then
            echo "  [✓] Tailscale successfully installed."
        else
            echo "  [!] Could not complete automated Tailscale installation."
            echo "      You can install it manually from: https://tailscale.com/download"
        fi
    else
        echo ""
        echo "************************************************************************"
        echo "  [NOTICE] Tailscale installation declined / skipped."
        echo ""
        echo "  The following features will NOT be enabled on this device:"
        echo "    ✗ Remote Web Terminal access from Nodexa Cloud"
        echo "    ✗ Live remote container log streaming and live log tailing"
        echo "    ✗ Remote container debugging and execution (nodexactl exec / SSH)"
        echo "    ✗ Zero-trust direct device tunnel / VPN mesh"
        echo ""
        echo "  Local container management and deployments will continue to work."
        echo "  You can install Tailscale anytime later: https://tailscale.com/download"
        echo "************************************************************************"
        echo ""
    fi
}

check_container_prerequisites
check_tailscale_prerequisite

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

configure_fleet() {
    if [ -z "${FLEET_ID}" ]; then
        if [ -t 0 ]; then
            printf "Enter Nodexa Fleet ID (leave empty to skip): "
            read -r FLEET_ID
        elif [ -r /dev/tty ]; then
            printf "Enter Nodexa Fleet ID (leave empty to skip): " >/dev/tty
            read -r FLEET_ID </dev/tty
        fi
    fi

    if [ -n "${FLEET_ID}" ]; then
        local cfg_dir="/etc/nodexa"
        if [ "${INSTALL_DIR}" != "/usr/local/bin" ] && [ -w "${INSTALL_DIR}" ]; then
            cfg_dir="${INSTALL_DIR}"
        else
            mkdir -p "${cfg_dir}" 2>/dev/null || sudo mkdir -p "${cfg_dir}"
        fi

        local cfg_file="${cfg_dir}/config.json"
        local json_content="{\"fleet_id\": \"${FLEET_ID}\"}"

        if [ -w "${cfg_dir}" ]; then
            echo "${json_content}" > "${cfg_file}"
            chmod 0644 "${cfg_file}"
        else
            echo "${json_content}" | sudo tee "${cfg_file}" >/dev/null
            sudo chmod 0644 "${cfg_file}"
        fi
        echo "==> Configured Fleet ID in ${cfg_file}"
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

    configure_fleet

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
            configure_fleet
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

configure_fleet

echo "==> Successfully installed ${BIN_NAME} to ${TARGET}"
echo "==> Alias ${ALIAS_NAME} linked to ${TARGET}"
"${TARGET}" version || "${TARGET}" --version || true
