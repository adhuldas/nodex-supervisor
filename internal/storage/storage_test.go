package storage

import (
	"testing"
	"time"
)

func TestAssembleAddsTheRemainderAsOther(t *testing.T) {
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	b := assemble([]Entry{
		{Path: "/var/lib/docker", Category: CategoryDocker, Bytes: 80},
		{Path: "/var/log", Category: CategoryLogs, Bytes: 8},
	}, 100, at)

	if len(b.Entries) != 3 || b.ScannedAt != at {
		t.Fatalf("breakdown = %+v", b)
	}
	other := b.Entries[2]
	if other.Path != "/other" || other.Category != CategoryOther || other.Bytes != 12 {
		t.Fatalf("other = %+v, want the 12 bytes the measured directories don't explain", other)
	}
	var sum uint64
	for _, e := range b.Entries {
		sum += e.Bytes
	}
	if sum != 100 {
		t.Fatalf("entries add up to %d, want the disk's used space (100)", sum)
	}
}

func TestAssembleNeverGoesNegative(t *testing.T) {
	// Files written while the scan ran can push the measured sum past what
	// statfs said was used.
	b := assemble([]Entry{{Path: "/usr", Category: CategoryOS, Bytes: 120}}, 100, time.Now())
	if got := b.Entries[1].Bytes; got != 0 {
		t.Fatalf("other = %d, want 0", got)
	}
}

func TestAssembleScalesAnImpossibleOvercount(t *testing.T) {
	// /home measured at far more than the disk holds (reflinked copies, say).
	b := assemble([]Entry{
		{Path: "/var/lib/docker", Category: CategoryDocker, Bytes: 100},
		{Path: "/home", Category: CategoryOther, Bytes: 900},
	}, 500, time.Now())

	var sum uint64
	for _, e := range b.Entries {
		sum += e.Bytes
	}
	if sum > 500 {
		t.Fatalf("entries add up to %d, more than the disk's used space (500)", sum)
	}
	if b.Entries[1].Bytes <= b.Entries[0].Bytes {
		t.Fatalf("scaling lost the proportions: %+v", b.Entries)
	}
}

func TestResultIsSentOnceAndResentWhenTheHeartbeatFails(t *testing.T) {
	s := NewScanner("/", "/var/lib/nodexa")
	if s.Pending() != nil {
		t.Fatal("pending before any scan")
	}

	first := &Breakdown{ScannedAt: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)}
	s.latest = first
	got := s.Pending()
	if got == nil || !got.ScannedAt.Equal(first.ScannedAt) {
		t.Fatalf("Pending = %+v", got)
	}
	// The heartbeat failed, so nothing was acknowledged: still pending.
	if s.Pending() == nil {
		t.Fatal("a result the cloud never confirmed was dropped")
	}

	s.Acknowledge(got)
	if s.Pending() != nil {
		t.Fatal("an acknowledged result is pending again")
	}

	// A newer scan is pending; acknowledging the older one doesn't hide it.
	second := &Breakdown{ScannedAt: first.ScannedAt.Add(15 * time.Minute)}
	s.latest = second
	s.Acknowledge(first)
	if p := s.Pending(); p == nil || !p.ScannedAt.Equal(second.ScannedAt) {
		t.Fatalf("Pending after a newer scan = %+v", p)
	}
}

func TestScannerLatest(t *testing.T) {
	s := NewScanner("/", "/var/lib/nodexa")
	if s.Latest() != nil {
		t.Fatal("latest before any scan should be nil")
	}

	b := &Breakdown{ScannedAt: time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)}
	s.latest = b
	got := s.Latest()
	if got == nil || !got.ScannedAt.Equal(b.ScannedAt) {
		t.Fatalf("Latest = %+v, want %+v", got, b)
	}
	// Calling Latest multiple times continues to return the latest scan without clearing
	gotAgain := s.Latest()
	if gotAgain == nil || !gotAgain.ScannedAt.Equal(b.ScannedAt) {
		t.Fatalf("Latest again = %+v, want %+v", gotAgain, b)
	}
}
