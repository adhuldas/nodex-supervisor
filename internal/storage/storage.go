// Package storage works out what is using a host's disk -- container
// engine data, logs, applications, the OS, home directories -- for the
// dashboard's "Storage usage" breakdown, which is sent along with the
// heartbeat.
//
// Measuring means walking whole directory trees (/var/lib/docker alone can
// hold millions of files), so it runs in the background on a slow schedule
// and throttles itself; the heartbeat only ever picks up the last finished
// result.
package storage

import (
	"context"
	"log"
	"sync"
	"time"
)

// Categories group paths in the dashboard's donut chart and legend.
const (
	CategoryDocker       = "docker" // container engine storage: Docker, containerd, the supervisor's own data
	CategoryLogs         = "logs"
	CategoryApplications = "applications"
	CategoryOS           = "os"
	CategoryOther        = "other"
)

const (
	// DefaultInterval is how often the disk is re-measured. Storage
	// composition moves slowly, and each scan costs real disk I/O.
	DefaultInterval = 15 * time.Minute

	// initialDelay keeps the first scan out of the agent's startup.
	initialDelay = 90 * time.Second

	// scanTimeout abandons a scan that takes unreasonably long; a partial
	// result would understate every directory, so none is kept.
	scanTimeout = 10 * time.Minute

	// otherPath is the entry for everything on the disk that isn't under a
	// measured directory.
	otherPath = "/other"
)

// Entry is the space one directory takes, in bytes of disk actually
// allocated (what `du` reports, not file lengths).
type Entry struct {
	Path     string `json:"path"`
	Category string `json:"category"`
	Bytes    uint64 `json:"bytes"`
}

// Breakdown is one finished scan. Entries add up to the disk's used space:
// "/other" holds whatever the measured directories don't.
type Breakdown struct {
	ScannedAt time.Time `json:"scanned_at"`
	Entries   []Entry   `json:"entries"`
}

// assemble turns the measured directories into a Breakdown by adding the
// "/other" entry: the disk's used space (usedBytes) minus what was measured.
func assemble(measured []Entry, usedBytes uint64, scannedAt time.Time) Breakdown {
	var sum uint64
	for _, e := range measured {
		sum += e.Bytes
	}
	var other uint64
	if usedBytes > sum { // files written during the scan can push the sum past it
		other = usedBytes - sum
	}
	entries := make([]Entry, 0, len(measured)+1)
	entries = append(entries, measured...)
	entries = append(entries, Entry{Path: otherPath, Category: CategoryOther, Bytes: other})
	return Breakdown{ScannedAt: scannedAt, Entries: entries}
}

// Scanner measures the disk periodically and remembers which result the
// cloud has already been sent.
type Scanner struct {
	diskPath string
	dataDir  string
	interval time.Duration

	mu     sync.Mutex
	latest *Breakdown
	acked  time.Time // ScannedAt of the newest result the cloud confirmed
}

// NewScanner measures the filesystem holding diskPath (the one the agent
// reports disk usage for); dataDir is the agent's own data directory, whose
// container storage is counted under CategoryDocker.
func NewScanner(diskPath, dataDir string) *Scanner {
	return &Scanner{diskPath: diskPath, dataDir: dataDir, interval: DefaultInterval}
}

// Run scans until ctx is cancelled. Call it in its own goroutine.
func (s *Scanner) Run(ctx context.Context) {
	timer := time.NewTimer(initialDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		s.scanOnce(ctx)
		timer.Reset(s.interval)
	}
}

func (s *Scanner) scanOnce(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, scanTimeout)
	defer cancel()

	started := time.Now()
	b, err := collect(ctx, s.diskPath, s.dataDir)
	if err != nil {
		log.Printf("warning: storage scan: %v", err)
		return
	}
	if b == nil { // platform with no breakdown
		return
	}
	log.Printf("storage scan finished in %s (%d directories)", time.Since(started).Round(time.Second), len(b.Entries))
	s.mu.Lock()
	s.latest = b
	s.mu.Unlock()
}

// Pending returns the newest scan the cloud hasn't confirmed yet, or nil.
func (s *Scanner) Pending() *Breakdown {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest == nil || !s.latest.ScannedAt.After(s.acked) {
		return nil
	}
	b := *s.latest
	return &b
}

// Acknowledge records that the cloud accepted b, so it isn't sent again.
// Call it only after a heartbeat carrying b succeeded: a failed one leaves
// b pending for the next.
func (s *Scanner) Acknowledge(b *Breakdown) {
	if b == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.ScannedAt.After(s.acked) {
		s.acked = b.ScannedAt
	}
}
