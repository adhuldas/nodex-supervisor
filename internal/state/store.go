// Package state implements nodexa-agent's small persistent key/value store,
// backed by a single JSON file under /var/lib/nodexa/state. It exists so
// that facts which must survive a reboot -- boot counters, last-known
// container list, etc. -- have one obvious, atomically-written home,
// independent of whatever is happening on the (potentially immutable) root
// filesystem.
package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const stateFileName = "state.json"

// Record is the on-disk shape of the state store.
type Record struct {
	BootCount   int               `json:"boot_count"`
	FirstBootAt time.Time         `json:"first_boot_at"`
	LastBootAt  time.Time         `json:"last_boot_at"`
	Values      map[string]string `json:"values"`
}

// Store is a thread-safe, file-backed key/value store with a few
// well-known boot-tracking fields promoted to first-class struct fields for
// clarity in nodexactl status output.
type Store struct {
	mu   sync.Mutex
	path string
	rec  Record
}

// Open loads (or initializes) the state store under dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, stateFileName)}

	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, &s.rec)
	}
	if s.rec.Values == nil {
		s.rec.Values = map[string]string{}
	}
	return s, nil
}

// RecordBoot increments the persistent boot counter and updates timestamps.
// Call once during agent startup. Returns the updated record.
func (s *Store) RecordBoot() (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	if s.rec.BootCount == 0 {
		s.rec.FirstBootAt = now
	}
	s.rec.BootCount++
	s.rec.LastBootAt = now

	if err := s.saveLocked(); err != nil {
		return s.rec, err
	}
	return s.rec, nil
}

// Get returns a stored value and whether it was present.
func (s *Store) Get(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.rec.Values[key]
	return v, ok
}

// Set stores a value and persists it immediately (atomic write).
func (s *Store) Set(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec.Values[key] = value
	return s.saveLocked()
}

// Snapshot returns a copy of the current record.
func (s *Store) Snapshot() Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.rec
	cp.Values = make(map[string]string, len(s.rec.Values))
	for k, v := range s.rec.Values {
		cp.Values[k] = v
	}
	return cp
}

// saveLocked writes the record atomically (write to temp file, fsync,
// rename) so a power loss mid-write cannot corrupt existing state.
func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.rec, "", "  ")
	if err != nil {
		return err
	}

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
