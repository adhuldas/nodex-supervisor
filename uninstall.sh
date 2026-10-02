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
