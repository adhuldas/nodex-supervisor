//go:build darwin

package storage

import (
	"context"
	"testing"
	"time"
)

func TestDarwinCollect(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping full host storage scan in short mode")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	b, err := collect(ctx, "/", "/var/lib/nodexa")
	if err != nil {
		t.Fatalf("collect error: %v", err)
	}
	if b == nil {
		t.Fatal("collect returned nil")
	}
	if len(b.Entries) == 0 {
		t.Fatal("collect returned no entries")
	}

	t.Logf("Breakdown ScannedAt: %v, Entries count: %d", b.ScannedAt, len(b.Entries))
	var totalMeasured uint64
	for _, e := range b.Entries {
		totalMeasured += e.Bytes
		t.Logf("  [%-12s] %-55s %12d bytes (%.2f GB)", e.Category, e.Path, e.Bytes, float64(e.Bytes)/(1024*1024*1024))
	}
	t.Logf("Total accounted for: %d bytes (%.2f GB)", totalMeasured, float64(totalMeasured)/(1024*1024*1024))
}
