#!/usr/bin/env bash
# uninstall.sh -- Uninstaller for nodex-supervisor (nodexa-agent)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/uninstall.sh | sh
#
# Or with custom directory:
#   ./uninstall.sh [--dir <install_dir>]

set -e

INSTALL_DIR="/usr/local/bin"
BIN_NAME="nodex-supervisor"
ALIAS_NAME="nodexa-agent"

while [ $# -gt 0 ]; do
    case "$1" in
        --dir)
            INSTALL_DIR="$2"
            shift 2
            ;;
        -h|--help)
            echo "Usage: $0 [--dir <path>]"
            exit 0
            ;;
        *)
            echo "Unknown argument: $1"
            exit 1
            ;;
    esac
done

echo "==> Uninstalling ${BIN_NAME} and ${ALIAS_NAME} from ${INSTALL_DIR}..."
removed=0

as_root() {
    if [ "$(id -u)" -eq 0 ]; then
        "$@"
    else
        sudo "$@"
    fi
}

# Stop and remove the boot service install.sh set up, before the binary goes.
SYSTEMD_UNIT="/etc/systemd/system/nodex-supervisor.service"
LAUNCHD_LABEL="com.nodexa.supervisor"
LAUNCHD_PLIST="/Library/LaunchDaemons/${LAUNCHD_LABEL}.plist"
if [ -f "${SYSTEMD_UNIT}" ]; then
    as_root systemctl disable --now nodex-supervisor 2>/dev/null || true
    as_root rm -f "${SYSTEMD_UNIT}"
    as_root systemctl daemon-reload 2>/dev/null || true
    echo "  Removed systemd service nodex-supervisor"
    removed=1
elif [ -f "${LAUNCHD_PLIST}" ]; then
    as_root launchctl bootout "system/${LAUNCHD_LABEL}" 2>/dev/null || true
    as_root rm -f "${LAUNCHD_PLIST}" /etc/newsyslog.d/nodex-supervisor.conf
    echo "  Removed launchd daemon ${LAUNCHD_LABEL}"
    removed=1
fi


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
