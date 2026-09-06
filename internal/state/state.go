// Package state holds the probe cache and the excuse ledger.
//
// The reconcile is otherwise stateless. Excuses exist only for files that
// would never converge — ffmpeg cannot process them, or the re-encode comes
// out larger — and are keyed on size and mtime, so a replaced file is judged
// fresh.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/edjeffreys/conform/internal/media"
)

type Record struct {
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`

	// Cached so a rescan costs one stat per unchanged file, not one ffprobe.
	Probe *media.File `json:"probe,omitempty"`

	Failures    int       `json:"failures,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastAttempt time.Time `json:"lastAttempt,omitempty"`

	Excused      bool   `json:"excused,omitempty"`
	ExcuseReason string `json:"excuseReason,omitempty"`
}

type Store struct {
	path    string
	mu      sync.Mutex
	records map[string]*Record
	dirty   bool
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "conform-state.json"), records: map[string]*Record{}}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.records); err != nil {
		// Nothing here is authoritative, so a corrupt file costs a re-probe
		// and nothing else.
		s.records = map[string]*Record{}
	}
	return s, nil
}

// A changed size or mtime discards the cached probe, the failure count and any
// excuse together — they all described the previous file.
func (s *Store) Get(path string, size int64, mod time.Time) *Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[path]
	if !ok {
		return nil
	}
	if r.Size != size || !r.ModTime.Equal(mod) {
		delete(s.records, path)
		s.dirty = true
		return nil
	}
	return r
}

func (s *Store) Put(path string, r *Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[path] = r
	s.dirty = true
}

// Forget is called after a replace: the new file must not inherit the old
// one's history.
func (s *Store) Forget(path string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, path)
	s.dirty = true
}

// Prune keeps a long-lived state file from growing as media is deleted.
func (s *Store) Prune(seen map[string]bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for p := range s.records {
		if !seen[p] {
			delete(s.records, p)
			n++
		}
	}
	if n > 0 {
		s.dirty = true
	}
	return n
}

// Written via a temp file and a rename, so a crash mid-write leaves the
// previous state intact rather than a truncated file.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("commit state: %w", err)
	}
	s.dirty = false
	return nil
}
