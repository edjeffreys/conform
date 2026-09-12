// Package state holds the probe cache and the excuse ledger.
//
// The reconcile is otherwise stateless. Excuses exist only for files that
// would never converge — ffmpeg cannot process them, or the re-encode comes
// out larger — and are keyed on size and mtime, so a replaced file is judged
// fresh. A larger re-encode excuses only the encode: the file is still held to
// everything a remux can do.
//
// The two are stored apart because they have different writers: one cache file
// written by whatever scans, and one excuse file per media file written by
// whichever process owns it.
package state

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/edjeffreys/conform/internal/media"
)

// Describes the file that was at the path when it was written, not the path.
type Excuse struct {
	// Duplicated into the contents because the filename is a hash of it.
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modTime"`

	Failures    int       `json:"failures,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastAttempt time.Time `json:"lastAttempt"`

	Excused bool   `json:"excused,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// Records written before this field existed excuse the remux too.
	EncodeOnly bool `json:"encodeOnly,omitempty"`
}

func (e *Excuse) describes(size int64, mod time.Time) bool {
	return e.Size == size && e.ModTime.Equal(mod)
}

type Store struct {
	cachePath string
	excuseDir string

	mu     sync.Mutex
	probes map[string]*media.File
	dirty  bool
}

func Open(dir string) (*Store, error) {
	s := &Store{
		// v2 added pix_fmt: older entries are re-probed, not read as unknown.
		cachePath: filepath.Join(dir, "probes-v2.json"),
		excuseDir: filepath.Join(dir, "excuses"),
		probes:    map[string]*media.File{},
	}
	if err := os.MkdirAll(s.excuseDir, 0o755); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.cachePath)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &s.probes); err != nil {
		// Nothing here is authoritative, so a corrupt file costs a re-probe
		// and nothing else.
		s.probes = map[string]*media.File{}
	}
	return s, nil
}

// A changed size or mtime discards the cached probe: it described a different
// file.
func (s *Store) Cached(path string, size int64, mod time.Time) *media.File {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.probes[path]
	if !ok {
		return nil
	}
	if f.Size != size || !f.ModTime.Equal(mod) {
		delete(s.probes, path)
		s.dirty = true
		return nil
	}
	return f
}

// Only the scanning process may call this — the cache has a single writer.
func (s *Store) PutProbe(f *media.File) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes[f.Path] = f
	s.dirty = true
}

func (s *Store) Excuse(path string, size int64, mod time.Time) *Excuse {
	p := s.excusePath(path)
	e, err := readExcuse(p)
	if err != nil || e == nil {
		return nil
	}
	if !e.describes(size, mod) {
		// Dropped here rather than by Prune, which only removes paths that
		// have vanished — this one still exists.
		os.Remove(p)
		return nil
	}
	return e
}

// Permanent, and does not count attempts: retrying produces the same output.
func (s *Store) Reject(path string, size int64, mod time.Time, reason string) error {
	return s.writeExcuse(&Excuse{
		Path: path, Size: size, ModTime: mod,
		LastAttempt: time.Now(), Excused: true, Reason: reason,
	})
}

func (s *Store) ExcuseEncode(path string, size int64, mod time.Time, reason string) error {
	return s.writeExcuse(&Excuse{
		Path: path, Size: size, ModTime: mod,
		LastAttempt: time.Now(), Excused: true, EncodeOnly: true, Reason: reason,
	})
}

func (s *Store) Fail(path string, size int64, mod time.Time, detail string, maxFailures int) error {
	e := s.Excuse(path, size, mod)
	if e == nil {
		e = &Excuse{Path: path, Size: size, ModTime: mod}
	}
	e.Failures++
	e.LastError = detail
	e.LastAttempt = time.Now()
	if maxFailures > 0 && e.Failures >= maxFailures {
		e.Excused = true
		e.EncodeOnly = false
		e.Reason = fmt.Sprintf("failed %d times, last: %s", e.Failures, truncate(detail, 200))
	}
	return s.writeExcuse(e)
}

// Called after a replace. Best-effort: a leftover excuse is keyed on the old
// size and mtime, so it is already ignored on the next read.
func (s *Store) Forget(path string) {
	s.mu.Lock()
	if _, ok := s.probes[path]; ok {
		delete(s.probes, path)
		s.dirty = true
	}
	s.mu.Unlock()
	os.Remove(s.excusePath(path))
}

// Keeps the cache and the excuse directory from growing as media is deleted.
func (s *Store) Prune(seen map[string]bool) int {
	n := 0

	s.mu.Lock()
	for p := range s.probes {
		if !seen[p] {
			delete(s.probes, p)
			s.dirty = true
			n++
		}
	}
	s.mu.Unlock()

	entries, err := os.ReadDir(s.excuseDir)
	if err != nil {
		return n
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		p := filepath.Join(s.excuseDir, entry.Name())
		e, err := readExcuse(p)
		if err != nil || e == nil || !seen[e.Path] {
			os.Remove(p)
			n++
		}
	}
	return n
}

// Flushes the cache only. Excuses are written as they happen, so a worker
// killed mid-run still leaves its verdict behind.
func (s *Store) Save() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	data, err := json.MarshalIndent(s.probes, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.cachePath, data); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

func (s *Store) excusePath(path string) string {
	sum := sha256.Sum256([]byte(path))
	return filepath.Join(s.excuseDir, hex.EncodeToString(sum[:8])+".json")
}

func (s *Store) writeExcuse(e *Excuse) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(s.excusePath(e.Path), data)
}

func readExcuse(path string) (*Excuse, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var e Excuse
	if err := json.Unmarshal(data, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Temp-and-rename so a crash leaves the previous content, not a truncated file.
// The .tmp suffix also keeps it out of Prune, which reads only .json.
func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("commit %s: %w", filepath.Base(path), err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
