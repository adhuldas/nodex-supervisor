#!/usr/bin/env bash
# install.sh -- One-line installer for nodex-supervisor (nodexa-agent)
# Installs directly to /usr/local/bin so it is immediately available on PATH.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.sh | sh -s -- --fleet <fleet_id>
#
# Or with options:
#   ./install.sh [--dir <install_dir>] [--version <tag_or_version>] [--fleet <id>] [--uninstall]

set -e

REPO="adhuldas/nodex-supervisor"
INSTALL_DIR="/usr/local/bin"
BIN_NAME="nodex-supervisor"
ALIAS_NAME="nodexa-agent"
TARGET_VERSION=""
FLEET_ID="${FLEET_ID:-}"
# nodexa-backend the device registers with; overridable for staging/self-hosted.
CLOUD_URL="${CLOUD_URL:-https://nodex.elzora.tech/backend}"
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
        --fleet-id|--fleet|-f)
            FLEET_ID="$2"
            shift 2
            ;;
        --cloud-url)
            CLOUD_URL="$2"
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
            echo "Usage: $0 [--dir <path>] [--version <version>] [--fleet <id>] [--cloud-url <url>] [--install-tailscale|--skip-tailscale] [--uninstall]"
            exit 0
            ;;
        *)
            echo "Unknown argument: $1"
            exit 1
            ;;
    esac
done

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"

SERVICE_NAME="nodex-supervisor"
SYSTEMD_UNIT="/etc/systemd/system/${SERVICE_NAME}.service"
LAUNCHD_LABEL="com.nodexa.supervisor"
LAUNCHD_PLIST="/Library/LaunchDaemons/${LAUNCHD_LABEL}.plist"
NEWSYSLOG_CONF="/etc/newsyslog.d/${SERVICE_NAME}.conf"
MACOS_LOG="/var/log/${SERVICE_NAME}.log"

as_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    else
        sudo "$@"
    fi
}

has_systemd() {
    [ "${OS}" = "linux" ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]
}

# Stops a running supervisor so its binary can be replaced.
stop_service() {
    if has_systemd && [ -f "${SYSTEMD_UNIT}" ]; then
        as_root systemctl stop "${SERVICE_NAME}" 2>/dev/null || true
    elif [ "${OS}" = "darwin" ] && [ -f "${LAUNCHD_PLIST}" ]; then
        as_root launchctl bootout "system/${LAUNCHD_LABEL}" 2>/dev/null || true
    fi
}

# Runs the supervisor now and on every boot, so the device registers with
# the cloud without anyone starting it by hand.
setup_service() {
    local bin="${INSTALL_DIR}/${BIN_NAME}"

    if has_systemd; then
        echo "==> Setting up systemd service ${SERVICE_NAME}..."
        # No WatchdogSec: the agent stops petting the watchdog whenever the
        # container runtime is unreachable, which on a third-party host
        # (Docker restarting, no runtime installed yet) would kill it in a
        # loop. Restart=always still covers crashes.
        as_root tee "${SYSTEMD_UNIT}" >/dev/null <<EOF
[Unit]
Description=Nodex Supervisor - Nodexa device agent
After=network-online.target docker.service containerd.service
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
ExecStart=${bin}
Restart=always
RestartSec=5
TimeoutStopSec=15
RuntimeDirectory=nodexa
RuntimeDirectoryPreserve=restart
StateDirectory=nodexa

[Install]
WantedBy=multi-user.target
EOF
        as_root systemctl daemon-reload
        as_root systemctl enable "${SERVICE_NAME}" >/dev/null 2>&1
        as_root systemctl restart "${SERVICE_NAME}"
        sleep 3
        if systemctl is-active --quiet "${SERVICE_NAME}"; then
            echo "  [✓] ${SERVICE_NAME} is running and will start on boot."
        else
            echo "  [!] ${SERVICE_NAME} did not stay running. Check: sudo journalctl -u ${SERVICE_NAME} -n 50"
        fi
        echo "      Logs: sudo journalctl -u ${SERVICE_NAME} -f"
    elif [ "${OS}" = "darwin" ]; then
        echo "==> Setting up launchd daemon ${LAUNCHD_LABEL}..."
        # launchd's default PATH lacks /usr/local/bin and /opt/homebrew/bin,
        # where the docker and nerdctl CLIs live.
        as_root tee "${LAUNCHD_PLIST}" >/dev/null <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LAUNCHD_LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${bin}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>${MACOS_LOG}</string>
    <key>StandardErrorPath</key>
    <string>${MACOS_LOG}</string>
</dict>
</plist>
EOF
        as_root chmod 0644 "${LAUNCHD_PLIST}"
        # Rotate the log at 1 MB, keeping 5 compressed copies.
        echo "${MACOS_LOG}  644  5  1024  *  JN" | as_root tee "${NEWSYSLOG_CONF}" >/dev/null
        as_root launchctl bootout "system/${LAUNCHD_LABEL}" 2>/dev/null || true
        as_root launchctl bootstrap system "${LAUNCHD_PLIST}"
        sleep 3
        if as_root launchctl print "system/${LAUNCHD_LABEL}" 2>/dev/null | grep -q 'state = running'; then
            echo "  [✓] ${SERVICE_NAME} is running and will start on boot."
        else
            echo "  [!] ${SERVICE_NAME} did not stay running. Check: tail -n 50 ${MACOS_LOG}"
        fi
        echo "      Logs: tail -f ${MACOS_LOG}"
    else
        echo "  [!] No systemd or launchd found; ${SERVICE_NAME} will not start automatically."
        echo "      Start it yourself with: sudo ${bin}"
    fi
}

remove_service() {
    if [ "${OS}" = "linux" ] && [ -f "${SYSTEMD_UNIT}" ]; then
        as_root systemctl disable --now "${SERVICE_NAME}" 2>/dev/null || true
        as_root rm -f "${SYSTEMD_UNIT}"
        as_root systemctl daemon-reload 2>/dev/null || true
        echo "  Removed systemd service ${SERVICE_NAME}"
    elif [ "${OS}" = "darwin" ] && [ -f "${LAUNCHD_PLIST}" ]; then
        as_root launchctl bootout "system/${LAUNCHD_LABEL}" 2>/dev/null || true
        as_root rm -f "${LAUNCHD_PLIST}" "${NEWSYSLOG_CONF}"
        echo "  Removed launchd daemon ${LAUNCHD_LABEL}"
    fi
}

if [ "${DO_UNINSTALL}" = "1" ]; then
    remove_service
    echo "==> Uninstalling ${BIN_NAME} and ${ALIAS_NAME} from ${INSTALL_DIR}..."
    removed=0
    # nodex only when it's our alias, never the Nodex cloud CLI.
    nodex_alias=""
    if [ -L "${INSTALL_DIR}/nodex" ] && [ "$(readlink "${INSTALL_DIR}/nodex")" = "${INSTALL_DIR}/nodexactl" ]; then
        nodex_alias="${INSTALL_DIR}/nodex"
    fi
    for target_bin in "${INSTALL_DIR}/${BIN_NAME}" "${INSTALL_DIR}/${ALIAS_NAME}" "${INSTALL_DIR}/nodexactl" "${INSTALL_DIR}/nodexa" ${nodex_alias:+"${nodex_alias}"}; do
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

    if [ "${INSTALL_DIR}" != "/usr/bin" ] && [ -d "/usr/bin" ]; then
        for b in "${BIN_NAME}" "${ALIAS_NAME}" "nodexactl" "nodexa" "nodex"; do
            if [ -L "/usr/bin/${b}" ]; then
                t="$(readlink "/usr/bin/${b}" 2>/dev/null || true)"
                if echo "${t}" | grep -q "${INSTALL_DIR}"; then
                    if [ -w "/usr/bin" ]; then
                        rm -f "/usr/bin/${b}"
                    else
                        sudo rm -f "/usr/bin/${b}" 2>/dev/null || true
                    fi
                fi
            fi
        done
    fi

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
        # Remote terminal and logs need the daemon running, now and after reboots.
        if [ "${OS}" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
            if [ "$(id -u)" -eq 0 ]; then
                systemctl enable --now tailscaled >/dev/null 2>&1 || true
            else
                sudo systemctl enable --now tailscaled >/dev/null 2>&1 || true
            fi
        fi
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

    stop_service
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

    # Also link to /usr/bin if INSTALL_DIR is not /usr/bin
    if [ "${INSTALL_DIR}" != "/usr/bin" ] && [ -d "/usr/bin" ]; then
        if [ -w "/usr/bin" ]; then
            ln -sf "${dest}" "/usr/bin/${BIN_NAME}" 2>/dev/null || true
            ln -sf "${dest}" "/usr/bin/${ALIAS_NAME}" 2>/dev/null || true
        elif command -v sudo >/dev/null 2>&1; then
            sudo ln -sf "${dest}" "/usr/bin/${BIN_NAME}" 2>/dev/null || true
            sudo ln -sf "${dest}" "/usr/bin/${ALIAS_NAME}" 2>/dev/null || true
        fi
    fi
}

# Installs the local CLI as nodexactl (same name as on Nodexa OS) with
# `nodexa` and `nodex` aliases. A `nodex` that isn't ours (the cloud CLI,
# from nodex-cli's installer) is kept rather than replaced.
install_cli() {
    local src="$1"
    if [ ! -f "${src}" ]; then
        echo "  [!] This release has no nodexactl; the local CLI was not installed."
        return 0
    fi
    if [ -w "${INSTALL_DIR}" ]; then
        cp -f "${src}" "${INSTALL_DIR}/nodexactl"
        chmod 0755 "${INSTALL_DIR}/nodexactl"
        ln -sf "${INSTALL_DIR}/nodexactl" "${INSTALL_DIR}/nodexa"
    else
        sudo cp -f "${src}" "${INSTALL_DIR}/nodexactl"
        sudo chmod 0755 "${INSTALL_DIR}/nodexactl"
        sudo ln -sf "${INSTALL_DIR}/nodexactl" "${INSTALL_DIR}/nodexa"
    fi
    local aliases="nodexa" try="nodexa"
    if nodex_alias_free; then
        if [ -w "${INSTALL_DIR}" ]; then
            ln -sf "${INSTALL_DIR}/nodexactl" "${INSTALL_DIR}/nodex"
        else
            sudo ln -sf "${INSTALL_DIR}/nodexactl" "${INSTALL_DIR}/nodex"
        fi
        aliases="nodexa, nodex"
        try="nodex"
    else
        echo "  [!] ${INSTALL_DIR}/nodex is another program (the Nodex cloud CLI?); left as is. Use nodexa instead."
    fi

    # Also link into /usr/bin if INSTALL_DIR is /usr/local/bin, ensuring nodex works
    # even when /usr/local/bin is missing from root's PATH (e.g. Yocto / minimal Linux)
    if [ "${INSTALL_DIR}" != "/usr/bin" ] && [ -d "/usr/bin" ]; then
        if [ -w "/usr/bin" ]; then
            ln -sf "${INSTALL_DIR}/nodexactl" "/usr/bin/nodexactl" 2>/dev/null || true
            ln -sf "${INSTALL_DIR}/nodexactl" "/usr/bin/nodexa" 2>/dev/null || true
            if [ ! -e "/usr/bin/nodex" ] || [ -L "/usr/bin/nodex" ]; then
                ln -sf "${INSTALL_DIR}/nodexactl" "/usr/bin/nodex" 2>/dev/null || true
            fi
        elif command -v sudo >/dev/null 2>&1; then
            sudo ln -sf "${INSTALL_DIR}/nodexactl" "/usr/bin/nodexactl" 2>/dev/null || true
            sudo ln -sf "${INSTALL_DIR}/nodexactl" "/usr/bin/nodexa" 2>/dev/null || true
            if [ ! -e "/usr/bin/nodex" ] || [ -L "/usr/bin/nodex" ]; then
                sudo ln -sf "${INSTALL_DIR}/nodexactl" "/usr/bin/nodex" 2>/dev/null || true
            fi
        fi
    fi

    echo "==> Installed CLI ${INSTALL_DIR}/nodexactl (aliases: ${aliases}). Try: sudo ${try} ps"
}

# True when INSTALL_DIR/nodex is absent, is a broken symlink, or already points to nodexactl.
nodex_alias_free() {
    local link="${INSTALL_DIR}/nodex"
    [ ! -e "${link}" ] && [ ! -L "${link}" ] && return 0
    if [ -L "${link}" ]; then
        local target="$(readlink "${link}" 2>/dev/null || true)"
        if [ ! -e "${link}" ] || [ "${target}" = "${INSTALL_DIR}/nodexactl" ] || [ "${target}" = "nodexactl" ]; then
            return 0
        fi
    fi
    return 1
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

    # Always written: without cloud_url the supervisor never registers.
    if [ -n "${CLOUD_URL}" ] || [ -n "${FLEET_ID}" ]; then
        local cfg_dir="/etc/nodexa"
        if [ "${INSTALL_DIR}" != "/usr/local/bin" ] && [ -w "${INSTALL_DIR}" ]; then
            cfg_dir="${INSTALL_DIR}"
        else
            mkdir -p "${cfg_dir}" 2>/dev/null || sudo mkdir -p "${cfg_dir}"
        fi

        local cfg_file="${cfg_dir}/config.json"
        local json_content="{\"cloud_url\": \"${CLOUD_URL%/}\""
        if [ -n "${FLEET_ID}" ]; then
            json_content="${json_content}, \"fleet_id\": \"${FLEET_ID}\""
        fi
        json_content="${json_content}}"

        if [ -w "${cfg_dir}" ]; then
            echo "${json_content}" > "${cfg_file}"
            chmod 0644 "${cfg_file}"
        else
            echo "${json_content}" | sudo tee "${cfg_file}" >/dev/null
            sudo chmod 0644 "${cfg_file}"
        fi
        echo "==> Configured cloud URL ${CLOUD_URL%/}${FLEET_ID:+ and Fleet ID ${FLEET_ID}} in ${cfg_file}"
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

    OS_VER="${VER}"
    if [ "${OS}" = "darwin" ]; then
        OS_VER="$(sysctl -n kern.osproductversion 2>/dev/null || sw_vers -productVersion 2>/dev/null || echo "${VER}")"
    fi

    LDFLAGS="-s -w \
      -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.AgentVersion=${VER} \
      -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.OSVersion=${OS_VER} \
      -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.Commit=${COMMIT} \
      -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.BuildDate=${DATE}"

    TMP_BIN="$(mktemp -t "${BIN_NAME}.XXXXXX")"
    CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" -o "${TMP_BIN}" ./cmd/nodexa-agent

    TARGET="${INSTALL_DIR}/${BIN_NAME}"
    run_install "${TMP_BIN}" "${TARGET}"
    rm -f "${TMP_BIN}"

    TMP_CLI="$(mktemp -t nodexactl.XXXXXX)"
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "${TMP_CLI}" ./cmd/nodexactl
    install_cli "${TMP_CLI}"
    rm -f "${TMP_CLI}"

    configure_fleet
    setup_service

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
            go install "github.com/${REPO}/cmd/nodexactl@latest" || true
            install_cli "$(go env GOPATH)/bin/nodexactl"
            configure_fleet
            setup_service
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

if [ ! -f "${TMP_DIR}/nodexactl" ] && command -v go >/dev/null 2>&1; then
    echo "  Release archive did not contain nodexactl; building locally with go..."
    (go install "github.com/${REPO}/cmd/nodexactl@latest" 2>/dev/null || true)
    GOPATH_CTL="$(go env GOPATH 2>/dev/null)/bin/nodexactl"
    if [ -f "${GOPATH_CTL}" ]; then
        cp -f "${GOPATH_CTL}" "${TMP_DIR}/nodexactl"
    fi
fi

install_cli "${TMP_DIR}/nodexactl"

configure_fleet
setup_service

echo "==> Successfully installed ${BIN_NAME} to ${TARGET}"
echo "==> Alias ${ALIAS_NAME} linked to ${TARGET}"
"${TARGET}" version || "${TARGET}" --version || true
