package state

import "testing"

func TestRecordBootIncrementsAcrossOpens(t *testing.T) {
	dir := t.TempDir()

	s1, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec1, err := s1.RecordBoot()
	if err != nil {
		t.Fatalf("RecordBoot: %v", err)
	}
	if rec1.BootCount != 1 {
		t.Fatalf("expected boot count 1, got %d", rec1.BootCount)
	}

	// Simulate a reboot: reopen the store from the same directory.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("Open (reboot): %v", err)
	}
	rec2, err := s2.RecordBoot()
	if err != nil {
		t.Fatalf("RecordBoot (reboot): %v", err)
	}
	if rec2.BootCount != 2 {
		t.Fatalf("expected boot count 2 after simulated reboot, got %d", rec2.BootCount)
	}
	if rec2.FirstBootAt != rec1.FirstBootAt {
		t.Fatalf("first boot timestamp should be preserved across reboots")
	}
}

func TestSetGetPersistsValue(t *testing.T) {
	dir := t.TempDir()

	s1, _ := Open(dir)
	if err := s1.Set("foo", "bar"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	s2, _ := Open(dir)
	v, ok := s2.Get("foo")
	if !ok || v != "bar" {
		t.Fatalf("expected persisted value bar, got %q (ok=%v)", v, ok)
	}
}

func TestGetMissingKey(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	if _, ok := s.Get("missing"); ok {
		t.Fatal("expected ok=false for missing key")
	}
}
