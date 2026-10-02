"""nodexa-agent service health and nodexactl <-> agent communication."""

import re


def test_agent_service_is_active(qemu):
    r = qemu.run_ok("systemctl is-active nodexa-agent.service")
    assert r.strip() == "active"


def test_agent_socket_exists_with_restrictive_permissions(qemu):
    r = qemu.run_ok("stat -c '%a %U %G' /run/nodexa/agent.sock")
    mode, owner, group = r.split()
    assert mode == "660", f"expected agent.sock mode 0660, got {mode}"
    assert group == "nodexa", f"expected agent.sock group 'nodexa', got {group!r}"


def test_agent_never_binds_tcp(qemu):
    """Security constraint: nodexa-agent must never expose a network
    service. There should be no listening TCP socket owned by it."""
    r = qemu.run("ss -tlnp 2>/dev/null | grep nodexa-agent || true")
    assert r.output.strip() == "", f"nodexa-agent should never bind TCP, found: {r.output!r}"


def test_nodexactl_status(qemu):
    out = qemu.run_ok("nodexactl status")
    assert "Nodexa OS Status" in out
    assert re.search(r"Device ID:\s+ndx_dev_[0-9a-f]{16}", out), out
    assert "Agent:" in out
    assert re.search(r"Agent:\s+Healthy", out), out


def test_nodexactl_version(qemu):
    out = qemu.run_ok("nodexactl version")
    assert "Nodexa OS" in out
    assert "Nodexa Agent" in out
    assert "Build Commit" in out


def test_nodexactl_health(qemu):
    out = qemu.run_ok("nodexactl health")
    assert "Nodexa OS Health" in out
    assert "Load Average" in out
    assert "Memory Used" in out
    assert "Disk Used" in out


def test_nodexactl_identity_matches_agent_state_file(qemu):
    out = qemu.run_ok("nodexactl identity")
    m = re.search(r"Device ID:\s+(\S+)", out)
    assert m, out
    device_id = m.group(1)

    on_disk = qemu.run_ok(
        "grep -o '\"device_id\":\"[^\"]*\"' /var/lib/nodexa/identity/identity.json "
        "|| grep -o '\"device_id\": *\"[^\"]*\"' /var/lib/nodexa/identity/identity.json"
    )
    assert device_id in on_disk, f"nodexactl reported {device_id!r}, on-disk identity: {on_disk!r}"


def test_agent_crash_is_automatically_recovered(qemu):
    """systemd's Restart=always + WatchdogSec should bring nodexa-agent
    back after it is killed, satisfying the crash-recovery acceptance
    criterion without any external supervisor."""
    pid_before = qemu.run_ok("systemctl show -p MainPID --value nodexa-agent.service")
    qemu.run_ok(f"kill -9 {pid_before.strip()}")

    def recovered(result):
        return result.output.strip() == "active"

    qemu.wait_for("systemctl is-active nodexa-agent.service", recovered, timeout=30, interval=2)

    pid_after = qemu.run_ok("systemctl show -p MainPID --value nodexa-agent.service")
    assert pid_after.strip() != pid_before.strip(), "expected a new PID after crash recovery"
    assert pid_after.strip() != "0"

    # The agent must come back reachable over its control socket too, not
    # just "active" per systemd.
    out = qemu.wait_for(
        "nodexactl status",
        lambda r: r.ok and "Nodexa OS Status" in r.output,
        timeout=30,
        interval=2,
    )
    assert "Nodexa OS Status" in out.output
