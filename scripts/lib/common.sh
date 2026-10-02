# Shared helpers sourced by scripts/*.sh. Not meant to be run directly.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

log()  { printf '\033[1;34m[nodexa]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[nodexa][warn]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[nodexa][error]\033[0m %s\n' "$*" >&2; exit 1; }

# nodexa_arch_to_machine <x86_64|arm64|beaglebone|variscite-imx6ul|generic-x86_64> -> MACHINE
# This now maps *targets*, not strictly CPU architectures -- beaglebone and
# variscite-imx6ul are both TARGET_ARCH "arm", not their own architecture,
# but each names exactly one MACHINE/kas file the same way x86_64/arm64 do.
nodexa_arch_to_machine() {
  case "$1" in
    x86_64)           echo "nodexa-qemu-x86_64" ;;
    arm64)            echo "nodexa-qemu-arm64" ;;
    beaglebone)       echo "nodexa-beaglebone" ;;
    variscite-imx6ul) echo "nodexa-variscite-imx6ul" ;;
    generic-x86_64)   echo "nodexa-generic-x86-64" ;;
    *) die "unsupported architecture '$1' (expected x86_64, arm64, beaglebone, variscite-imx6ul, or generic-x86_64)" ;;
  esac
}

# nodexa_is_qemu_machine <MACHINE> -> true (0) for the two QEMU-only Phase 1
# targets, false (1) for real-hardware Phase 1.5 targets (beaglebone,
# variscite-imx6ul) -- both of those ship a genuine flashable .wic despite
# also supporting a QEMU smoke-test boot path, so they must not be named
# like the QEMU-only artifacts. See scripts/build.sh's artifact naming.
nodexa_is_qemu_machine() {
  case "$1" in
    nodexa-qemu-*) return 0 ;;
    *) return 1 ;;
  esac
}

# Each OS product has its own version file: Nodex Nomad (the generic x86-64
# USB machine) is released on its own cadence from NOMAD_VERSION, every
# other machine from VERSION. AGENT_VERSION (supervisor) and
# src/nodexa-esp32/VERSION (ESP32 firmware) are separate again.
nodexa_version_file() {
  echo "${REPO_ROOT}/AGENT_VERSION"
}

nodexa_version() {
  tr -d '[:space:]' < "$(nodexa_version_file)" 2>/dev/null || echo "0.3.7"
}

# nodexa_artifact_basename <arch> <version> -> release file name, sans
# extension. Nodex Nomad is its own product, so it's named after it
# (nodex-os-nomad-<version>-x86_64); QEMU-only images are marked as such so
# they can't be mistaken for a flashable .wic.
nodexa_artifact_basename() {
  case "$1" in
    generic-x86_64) echo "nodex-os-nomad-${2}-x86_64" ;;
    x86_64|arm64)   echo "nodexa-os-${2}-qemu-${1}" ;;
    *)              echo "nodexa-os-${2}-${1}" ;;
  esac
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "required command '$1' not found in PATH"
}
# The container engine kas-container will use, and the kas image tag that
# matches the vendored scripts/kas-container (kept in sync automatically so
# helper containers never drift from the one kas itself runs).
nodexa_container_engine() {
  if [[ -n "${KAS_CONTAINER_ENGINE:-}" ]]; then echo "${KAS_CONTAINER_ENGINE}"
  elif command -v docker >/dev/null 2>&1; then echo docker
  elif command -v podman >/dev/null 2>&1; then echo podman
  else die "Docker or Podman is required"; fi
}

nodexa_kas_image() {
  local v
  v="$(sed -n 's/^KAS_CONTAINER_SCRIPT_VERSION="\(.*\)"/\1/p' "${REPO_ROOT}/scripts/kas-container")"
  echo "ghcr.io/siemens/kas/kas:${v:-5.5}"
}

# True when <dir> lives on a case-insensitive filesystem (the macOS APFS
# default). Probes for real rather than checking `uname`, so a developer who
# does put the repo on a case-sensitive volume gets the fast path.
nodexa_fs_is_case_insensitive() {
  local dir="$1" probe lower rc=1
  mkdir -p "$dir"
  probe="$(mktemp "${dir}/NodexaCaseProbe.XXXXXX")"
  lower="$(dirname "$probe")/$(basename "$probe" | tr '[:upper:]' '[:lower:]')"
  [[ -e "$lower" ]] && rc=0
  rm -f "$probe"
  return $rc
}

# Yocto refuses to build with TMPDIR on a case-insensitive filesystem, which
# rules out bind-mounting the build directory straight off macOS. When we
# detect one, bitbake's TMPDIR is redirected into a container-engine named
# volume (ext4 inside the engine's Linux VM, always case-sensitive) mounted at
# /build/tmp. conf/, cache/ and the rest of the build directory stay
# host-visible; only tmp/ moves. Sets, for the caller:
#
#   NODEXA_TMPDIR_VOLUME    volume name, or "" when the host FS is fine
#   NODEXA_KAS_ARGS         extra scripts/kas-container args (possibly empty)
#
# Expand NODEXA_KAS_ARGS as ${NODEXA_KAS_ARGS[@]+"${NODEXA_KAS_ARGS[@]}"} --
# macOS ships bash 3.2, where "${empty[@]}" trips `set -u`.
NODEXA_TMPDIR_VOLUME=""
NODEXA_KAS_ARGS=()
nodexa_use_tmpdir_volume() {
  local machine="$1"
  nodexa_fs_is_case_insensitive "${REPO_ROOT}/build" || return 0
  NODEXA_TMPDIR_VOLUME="nodexa-tmp-${machine}"
  NODEXA_KAS_ARGS=(--runtime-args "-v ${NODEXA_TMPDIR_VOLUME}:/build/tmp")

  # A freshly created volume is root-owned, but kas-container's entrypoint
  # runs bitbake as a user matching the host's uid/gid, so hand the volume
  # over before the first build tries to write to it.
  local engine
  engine="$(nodexa_container_engine)"
  if ! "$engine" volume inspect "${NODEXA_TMPDIR_VOLUME}" >/dev/null 2>&1; then
    "$engine" volume create "${NODEXA_TMPDIR_VOLUME}" >/dev/null
    "$engine" run --rm --user 0:0 -v "${NODEXA_TMPDIR_VOLUME}:/vol" \
      --entrypoint /bin/sh "$(nodexa_kas_image)" \
      -c "chown $(id -u):$(id -g) /vol" >/dev/null
  fi

  log "Case-insensitive filesystem: bitbake TMPDIR -> volume ${NODEXA_TMPDIR_VOLUME}"
}
