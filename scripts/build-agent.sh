#!/usr/bin/env bash
# scripts/build-agent.sh - standalone cross-compiler for nodexa-agent & nodexactl
#
# Builds static Linux binaries for BeagleBone, ARM64, x86_64, or host,
# generating release tarballs, sha256 checksums, and metadata manifests
# suitable for uploading to Nodexa Cloud as OTA update packages.
#
# Usage:
#   ./scripts/build-agent.sh [--arch beaglebone|armv7|arm64|x86_64|host|all]
#                            [--version X.Y.Z]
#                            [--agent-version X.Y.Z]
#                            [--out-dir <path>]
#                            [--no-ctl]
#
# Examples:
#   ./scripts/build-agent.sh --arch beaglebone
#   ./scripts/build-agent.sh --arch all --version 0.2.0
#   ./scripts/build-agent.sh --arch host
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/lib/common.sh"

require_cmd go

TARGET_ARCH="beaglebone"
VERSION_OVERRIDE=""
AGENT_VERSION_OVERRIDE=""
BUILD_CTL="true"
OUT_DIR="${REPO_ROOT}/build"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --arch) TARGET_ARCH="$2"; shift 2 ;;
    --arch=*) TARGET_ARCH="${1#*=}"; shift ;;
    --version) VERSION_OVERRIDE="$2"; shift 2 ;;
    --version=*) VERSION_OVERRIDE="${1#*=}"; shift ;;
    --agent-version) AGENT_VERSION_OVERRIDE="$2"; shift 2 ;;
    --agent-version=*) AGENT_VERSION_OVERRIDE="${1#*=}"; shift ;;
    --out-dir) OUT_DIR="$2"; shift 2 ;;
    --out-dir=*) OUT_DIR="${1#*=}"; shift ;;
    --no-ctl) BUILD_CTL="false"; shift ;;
    -h|--help)
      echo "Usage: $0 [--arch beaglebone|armv7|arm64|x86_64|host|all] [--version X.Y.Z] [--agent-version X.Y.Z] [--out-dir <path>] [--no-ctl]"
      exit 0
      ;;
    *) die "unknown argument: $1" ;;
  esac
done

OS_VERSION="${VERSION_OVERRIDE:-$(nodexa_version)}"
AGENT_VERSION="${AGENT_VERSION_OVERRIDE:-${OS_VERSION}}"
GIT_COMMIT="$(git -C "${REPO_ROOT}" rev-parse --short=12 HEAD 2>/dev/null || echo "unknown")"
BUILD_DATE="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"

BIN_DIR="${OUT_DIR}/bin"
ARTIFACTS_DIR="${OUT_DIR}/artifacts"
mkdir -p "${BIN_DIR}" "${ARTIFACTS_DIR}"

build_target() {
  local arch_name="$1"
  local target_goos="linux"
  local target_goarch=""
  local target_goarm=""
  local display_arch=""

  case "$arch_name" in
    beaglebone|armv7|arm|variscite-imx6ul)
      target_goarch="arm"
      target_goarm="7"
      display_arch="armv7"
      ;;
    arm64|aarch64)
      target_goarch="arm64"
      target_goarm=""
      display_arch="arm64"
      ;;
    x86_64|amd64)
      target_goarch="amd64"
      target_goarm=""
      display_arch="x86_64"
      ;;
    host)
      target_goos="$(go env GOOS)"
      target_goarch="$(go env GOARCH)"
      target_goarm="$(go env GOARM)"
      display_arch="${target_goarch}"
      ;;
    *)
      die "unsupported target architecture '${arch_name}'"
      ;;
  esac

  log "Building nodexa-agent for ${target_goos}/${display_arch} (Agent ${AGENT_VERSION}, OS ${OS_VERSION})"

  local agent_bin_name="nodexa-agent-${AGENT_VERSION}-${target_goos}-${display_arch}"
  local agent_bin_path="${BIN_DIR}/${agent_bin_name}"

  local enc_cloud_url=""
  local enc_ts_key=""
  if [[ -n "${CLOUD_URL:-}" ]]; then
    enc_cloud_url="$(python3 -c "import sys, base64; raw = bytearray(sys.argv[1].encode('utf-8')); sys.stdout.write('enc:' + base64.b64encode(bytes([b ^ 0x5a for b in raw])).decode('utf-8'))" "${CLOUD_URL}")"
  fi
  local ts_raw="${TAILSCALE_AUTHKEY:-${NODEXA_TAILSCALE_AUTHKEY:-}}"
  if [[ -n "${ts_raw}" ]]; then
    enc_ts_key="$(python3 -c "import sys, base64; raw = bytearray(sys.argv[1].encode('utf-8')); sys.stdout.write('enc:' + base64.b64encode(bytes([b ^ 0x5a for b in raw])).decode('utf-8'))" "${ts_raw}")"
  fi

  local ldflags="-s -w \
    -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.OSVersion=${OS_VERSION} \
    -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.AgentVersion=${AGENT_VERSION} \
    -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.Commit=${GIT_COMMIT} \
    -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.BuildDate=${BUILD_DATE} \
    -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.DefaultCloudURL=${enc_cloud_url} \
    -X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.DefaultTailscaleAuthKey=${enc_ts_key}"

  local agent_src_dir="${REPO_ROOT}"
  if [[ -d "${REPO_ROOT}/src/nodexa-agent" ]]; then
    agent_src_dir="${REPO_ROOT}/src/nodexa-agent"
  fi

  # Build standalone static agent binary
  (
    cd "${agent_src_dir}"
    export CGO_ENABLED=0
    export GOOS="${target_goos}"
    export GOARCH="${target_goarch}"
    if [[ -n "${target_goarm}" ]]; then
      export GOARM="${target_goarm}"
    fi
    go build -trimpath -ldflags "${ldflags}" -o "${agent_bin_path}" ./cmd/nodexa-agent
  )

  # Checksum for raw binary
  local bin_sha256
  if command -v sha256sum >/dev/null 2>&1; then
    bin_sha256="$(sha256sum "${agent_bin_path}" | awk '{print $1}')"
  else
    bin_sha256="$(shasum -a 256 "${agent_bin_path}" | awk '{print $1}')"
  fi
  echo "${bin_sha256}  ${agent_bin_name}" > "${BIN_DIR}/${agent_bin_name}.sha256"

  # Build nodexactl if requested and present
  local ctl_bin_name=""
  if [[ "${BUILD_CTL}" == "true" && -d "${REPO_ROOT}/src/nodexactl" ]]; then
    ctl_bin_name="nodexactl-${AGENT_VERSION}-${target_goos}-${display_arch}"
    local ctl_bin_path="${BIN_DIR}/${ctl_bin_name}"
    local ctl_ldflags="-s -w \
      -X github.com/nodexa/nodexa-os/nodexactl/internal/version.OSVersion=${OS_VERSION} \
      -X github.com/nodexa/nodexa-os/nodexactl/internal/version.AgentVersion=${AGENT_VERSION} \
      -X github.com/nodexa/nodexa-os/nodexactl/internal/version.Commit=${GIT_COMMIT} \
      -X github.com/nodexa/nodexa-os/nodexactl/internal/version.BuildDate=${BUILD_DATE}"

    (
      cd "${REPO_ROOT}/src/nodexactl"
      export CGO_ENABLED=0
      export GOOS="${target_goos}"
      export GOARCH="${target_goarch}"
      if [[ -n "${target_goarm}" ]]; then
        export GOARM="${target_goarm}"
      fi
      go build -trimpath -ldflags "${ctl_ldflags}" -o "${ctl_bin_path}" ./cmd/nodexactl
    )
  fi

  # Create packaging staging dir for OTA artifact
  local stage_dir
  stage_dir="$(mktemp -d "${OUT_DIR}/stage-XXXXXX")"
  cp "${agent_bin_path}" "${stage_dir}/nodexa-agent"
  chmod 0755 "${stage_dir}/nodexa-agent"

  if [[ "${BUILD_CTL}" == "true" && -n "${ctl_bin_name}" ]]; then
    cp "${BIN_DIR}/${ctl_bin_name}" "${stage_dir}/nodexactl"
    chmod 0755 "${stage_dir}/nodexactl"
  fi

  local tar_name="nodexa-agent-${AGENT_VERSION}-${target_goos}-${display_arch}.tar.gz"
  local tar_path="${ARTIFACTS_DIR}/${tar_name}"
  tar -czf "${tar_path}" -C "${stage_dir}" .
  rm -rf "${stage_dir}"

  # Checksum for tarball
  local tar_sha256
  if command -v sha256sum >/dev/null 2>&1; then
    tar_sha256="$(sha256sum "${tar_path}" | awk '{print $1}')"
  else
    tar_sha256="$(shasum -a 256 "${tar_path}" | awk '{print $1}')"
  fi
  echo "${tar_sha256}  ${tar_name}" > "${ARTIFACTS_DIR}/${tar_name}.sha256"

  # Manifest JSON for cloud upload
  local manifest_path="${ARTIFACTS_DIR}/nodexa-agent-${AGENT_VERSION}-${target_goos}-${display_arch}.json"
  cat > "${manifest_path}" <<EOF
{
  "name": "nodexa-agent",
  "version": "${AGENT_VERSION}",
  "os_version": "${OS_VERSION}",
  "target_arch": "${display_arch}",
  "goos": "${target_goos}",
  "goarch": "${target_goarch}",
  "goarm": "${target_goarm}",
  "binary_file": "${agent_bin_name}",
  "binary_sha256": "${bin_sha256}",
  "archive_file": "${tar_name}",
  "archive_sha256": "${tar_sha256}",
  "build_date": "${BUILD_DATE}",
  "commit": "${GIT_COMMIT}"
}
EOF

  log "Successfully built OTA release artifact:"
  log "  -> Binary:   ${agent_bin_path} (${bin_sha256:0:16}...)"
  log "  -> Archive:  ${tar_path} (${tar_sha256:0:16}...)"
  log "  -> Manifest: ${manifest_path}"
}

if [[ "${TARGET_ARCH}" == "all" ]]; then
  build_target "beaglebone"
  build_target "arm64"
  build_target "x86_64"
else
  build_target "${TARGET_ARCH}"
fi

log "Done! Artifacts ready in ${ARTIFACTS_DIR} for uploading to Nodexa Cloud."
