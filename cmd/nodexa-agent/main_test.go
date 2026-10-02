package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/state"
)

func TestAcquireInstanceLockRejectsSecondAgent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run", "agent.lock")

	first, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if second, err := acquireInstanceLock(path); err == nil {
		second.Close()
		t.Fatal("second lock succeeded while the first is held")
	}

	first.Close()
	again, err := acquireInstanceLock(path)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	again.Close()
}

func TestResolveFleetIDPrefersTransferredFleet(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fleetA, fleetC := "fleet-a", "fleet-c"

	if got := resolveFleetID(store, &fleetA); got != &fleetA {
		t.Fatalf("no transfer stored: got %v", derefString(got))
	}

	_ = store.Set(stateKeyFleetIDProvisioned, fleetA)
	_ = store.Set(stateKeyFleetID, "fleet-b")
	if got := derefString(resolveFleetID(store, &fleetA)); got != "fleet-b" {
		t.Fatalf("after transfer: got %q, want fleet-b", got)
	}

	// Re-provisioned into another fleet: the flash-time config wins.
	if got := derefString(resolveFleetID(store, &fleetC)); got != "fleet-c" {
		t.Fatalf("after re-provisioning: got %q, want fleet-c", got)
	}
}

func TestNewerVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.3.1", "0.3.0", true},
		{"0.3.0", "0.3.1", false},
		{"0.3.1", "0.3.1", false},
		{"0.10.0", "0.9.9", true},
		{"0.3", "0.3.0", false},
		{"v1.0.0", "0.9.0", true},
	} {
		if got := newerVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestMaybeDelegateToOTARemovesOlderBinary(t *testing.T) {
	dir := t.TempDir()
	ota := filepath.Join(dir, "nodexa-agent")
	attempt := filepath.Join(dir, "attempt")
	script := "#!/bin/sh\necho 'nodexa-agent 0.0.1 (os test, commit test)'\n"
	if err := os.WriteFile(ota, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	maybeDelegateToOTA(ota, attempt) // would exec (and never return) if it delegated
	if _, err := os.Stat(ota); !os.IsNotExist(err) {
		t.Fatalf("older OTA binary kept: %v", err)
	}
}
