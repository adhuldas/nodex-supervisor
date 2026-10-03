# nodex-supervisor

[![Release](https://github.com/adhuldas/nodex-supervisor/actions/workflows/release.yml/badge.svg)](https://github.com/adhuldas/nodex-supervisor/actions/workflows/release.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/adhuldas/nodex-supervisor)](https://goreportcard.com/report/github.com/adhuldas/nodex-supervisor)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

`nodex-supervisor` (also known as `nodexa-agent`) is the trusted device supervisor daemon for **Nodexa OS**. It is the central long-running service responsible for device identity, container workload management, system health monitoring, Wi-Fi provisioning, and safe Over-The-Air (OTA) updates.

---

## Installation

### Quick Install (macOS & Linux)

Installs `nodex-supervisor` to `/usr/local/bin`, prompts for your Fleet ID, links the compatibility alias `nodexa-agent`, and starts it as a service that also starts on every boot (systemd on Linux, launchd on macOS). The device registers with the cloud as soon as the service starts:

```bash
curl -fsSL https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.sh | sh
```

You can also pass the Fleet ID non-interactively (useful for unattended installs):
```bash
curl -fsSL https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.sh | sh -s -- --fleet <your-fleet-id>
```

Or from a local checkout:
```bash
./install.sh --fleet <your-fleet-id>
```

The supervisor registers with `https://nodex.elzora.tech/backend` by default. Point it at another backend with `--cloud-url <url>` (`-CloudUrl <url>` on Windows); the installer writes both values to `config.json`.

### Quick Install (Windows)

Installs `nodex-supervisor.exe` and `nodexa-agent.exe` to `$LOCALAPPDATA\nodex-supervisor\bin`, prompts for your Fleet ID, adds it to your user `PATH`, and starts it through a `nodex-supervisor` scheduled task. Run from an elevated (Administrator) PowerShell, the task starts at every boot; otherwise it starts when you log on:

```powershell
irm https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.ps1 | iex
```

You can also pass the Fleet ID non-interactively (useful for unattended installs):
```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/install.ps1))) -FleetId <your-fleet-id>
```

Or from a local checkout:
```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1 -FleetId <your-fleet-id>
```

### With Go

```bash
go install github.com/adhuldas/nodex-supervisor/cmd/nodexa-agent@latest
```

### Build from Source

```bash
git clone https://github.com/adhuldas/nodex-supervisor.git
cd nodex-supervisor
go build -trimpath -o /usr/local/bin/nodex-supervisor ./cmd/nodexa-agent
ln -sf /usr/local/bin/nodex-supervisor /usr/local/bin/nodexa-agent
```

Verify the installation:

```bash
nodex-supervisor --version
# nodex-supervisor 0.3.8 (commit 96be907cc8d4, built 2026-10-03T09:11:40Z)
```

---

## Uninstallation

If you need to uninstall `nodex-supervisor`:

### macOS & Linux

Using the uninstaller script:
```bash
curl -fsSL https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/uninstall.sh | sh
```

Or using the install script flag:
```bash
./install.sh --uninstall
```

### Windows (PowerShell)

Using the uninstaller script:
```powershell
irm https://raw.githubusercontent.com/adhuldas/nodex-supervisor/main/uninstall.ps1 | iex
```

Or using the install script parameter:
```powershell
powershell -ExecutionPolicy Bypass -File .\install.ps1 -Uninstall
```

---

## Key Capabilities

- **Device Identity**: Generates and securely maintains immutable device credentials (`ndx_dev_...`) derived from hardware UUID / CPU serials (with QEMU/emulation fallback).
- **Container Lifecycle Engine**: Native OCI/runc container orchestration with custom bridge networking, cgroup v2 resource limits, container health monitors, and automatic restart policies.
- **Fail-Safe OTA Updates**: Orchestrates dual-slot (A/B) root filesystem upgrades and in-place supervisor updates with automatic crash-loop boot counters and fallback rollbacks.
- **System Health & Watchdog**: Monitors load averages, memory consumption, storage margins, and local networking verdicts; integrates directly with systemd watchdog notifications.
- **Local Control API**: Serves a secure, Unix domain socket (`/run/nodexa/agent.sock` with restrictive permissions) for local host queries and CLI commands (`nodexactl`).
- **Fleet Management**: Heartbeats device telemetry and state changes back to Nodexa Cloud over secure TLS.

---

## Usage

### Running as a Service

The install scripts set the service up and start it; re-running them updates the binary and restarts it, and uninstalling removes it.

| Platform | Service | Status | Logs |
|----------|---------|--------|------|
| Linux (systemd) | `nodex-supervisor.service` | `systemctl status nodex-supervisor` | `journalctl -u nodex-supervisor -f` |
| macOS (launchd) | `com.nodexa.supervisor` | `sudo launchctl print system/com.nodexa.supervisor` | `tail -f /var/log/nodex-supervisor.log` |
| Windows | `nodex-supervisor` scheduled task | `Get-ScheduledTask nodex-supervisor` | `$LOCALAPPDATA\nodex-supervisor\bin\nodex-supervisor.log` |

On Linux without systemd the installer can't start it automatically; run it manually (below) under your init system.

On Nodexa OS itself the agent runs as `nodexa-agent.service`.

### Running Manually

```bash
nodex-supervisor
```

When started, `nodex-supervisor`:
1. Acquires an exclusive file lock to prevent duplicate supervisor instances.
2. Initializes or restores the device identity.
3. Sets up the control socket at `/run/nodexa/agent.sock` (`/var/run/nodexa/agent.sock` on macOS).
4. Connects to the configured cloud endpoint and begins health monitoring.

### Checking Version

```bash
nodex-supervisor --version
# nodex-supervisor 0.3.8 (commit 96be907cc8d4, built 2026-10-03T09:11:40Z)
```

---

## Development

### Running Tests

Run unit and integration tests across all packages:

```bash
go test ./...
```

### Standalone Cross-Compilation

Use the included build script to cross-compile static binaries and OTA package tarballs for BeagleBone, ARM64, x86_64, or the host architecture:

```bash
# Build for host
./scripts/build-agent.sh --arch host --no-ctl

# Build for BeagleBone (ARMv7)
./scripts/build-agent.sh --arch beaglebone --no-ctl

# Build for all supported Linux architectures
./scripts/build-agent.sh --arch all --no-ctl
```

---

## Versioning & Releases

Project releases are strictly controlled by the [`AGENT_VERSION`](AGENT_VERSION) file in the repository root.

- When changes are merged to `main`, GitHub Actions reads `AGENT_VERSION`.
- If a release for `v<AGENT_VERSION>` already exists, the workflow automatically skips execution.
- To cut a new release across all platforms (Darwin, Linux ARM/ARM64/x86_64, Windows), simply bump the version string in `AGENT_VERSION` and push to `main`.

---

## License

This project is licensed under the Apache License 2.0. See [LICENSE](LICENSE) for details.