package state

import (
	"testing"
	"time"

	"github.com/edjeffreys/conform/internal/media"
)

// Under one shared file each process wrote the whole map, so whichever saved
// last erased the other's verdict — and a worker is one process per file.
func TestSeparateProcessesDoNotLoseEachOthersExcuses(t *testing.T) {
	dir := t.TempDir()
	mod := time.Now().Truncate(time.Second)

	a, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	if err := a.Reject("/m/a.mkv", 1, mod, "re-encode was larger"); err != nil {
		t.Fatal(err)
	}
	if err := b.Reject("/m/b.mkv", 2, mod, "ffmpeg could not read it"); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e := again.Excuse("/m/a.mkv", 1, mod); e == nil || !e.Excused {
		t.Error("the first writer's excuse was lost")
	}
	if e := again.Excuse("/m/b.mkv", 2, mod); e == nil || !e.Excused {
		t.Error("the second writer's excuse was lost")
	}
}

// An excuse describes the file that was at a path, not the path itself, or a
// new download inherits the previous one's verdict and is never processed.
func TestChangedFileForgetsItsExcuse(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Truncate(time.Second)
	if err := s.Reject("/m/x.mkv", 100, mod, "too big"); err != nil {
		t.Fatal(err)
	}

	if e := s.Excuse("/m/x.mkv", 100, mod); e == nil || !e.Excused {
		t.Fatal("excuse did not survive an unchanged file")
	}
	if e := s.Excuse("/m/x.mkv", 200, mod); e != nil {
		t.Error("excuse survived a size change")
	}

	if err := s.Fail("/m/y.mkv", 100, mod, "boom", 5); err != nil {
		t.Fatal(err)
	}
	if e := s.Excuse("/m/y.mkv", 100, mod.Add(time.Second)); e != nil {
		t.Error("failure count survived an mtime change")
	}
}

func TestFailEscalatesToExcusedAtTheThreshold(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Truncate(time.Second)

	for i := 1; i <= 2; i++ {
		if err := s.Fail("/m/x.mkv", 1, mod, "boom", 3); err != nil {
			t.Fatal(err)
		}
		e := s.Excuse("/m/x.mkv", 1, mod)
		if e == nil {
			t.Fatalf("attempt %d recorded nothing", i)
		}
		if e.Failures != i {
			t.Errorf("after attempt %d, Failures = %d", i, e.Failures)
		}
		if e.Excused {
			t.Errorf("excused after %d of 3 attempts", i)
		}
	}

	if err := s.Fail("/m/x.mkv", 1, mod, "boom", 3); err != nil {
		t.Fatal(err)
	}
	if e := s.Excuse("/m/x.mkv", 1, mod); e == nil || !e.Excused {
		t.Error("not excused after reaching the threshold")
	}
}

func TestExcusesPersistWithoutSave(t *testing.T) {
	dir := t.TempDir()
	mod := time.Now().Truncate(time.Second)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Reject("/m/x.mkv", 1, mod, "too big"); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if e := again.Excuse("/m/x.mkv", 1, mod); e == nil || e.Reason != "too big" {
		t.Error("excuse did not survive without an explicit Save")
	}
}

func TestProbeCacheRoundTrips(t *testing.T) {
	dir := t.TempDir()
	mod := time.Now().Truncate(time.Second)

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.PutProbe(&media.File{Path: "/m/x.mkv", Size: 42, ModTime: mod, Container: "mkv"})
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	f := again.Cached("/m/x.mkv", 42, mod)
	if f == nil {
		t.Fatal("cached probe did not survive a reopen")
	}
	if f.Container != "mkv" {
		t.Errorf("Container = %q, want mkv", f.Container)
	}
	if again.Cached("/m/x.mkv", 43, mod) != nil {
		t.Error("cached probe survived a size change")
	}
}

func TestForgetClearsBothHalves(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Truncate(time.Second)
	s.PutProbe(&media.File{Path: "/m/x.mkv", Size: 1, ModTime: mod})
	if err := s.Reject("/m/x.mkv", 1, mod, "too big"); err != nil {
		t.Fatal(err)
	}

	s.Forget("/m/x.mkv")

	if s.Cached("/m/x.mkv", 1, mod) != nil {
		t.Error("cached probe survived Forget")
	}
	if s.Excuse("/m/x.mkv", 1, mod) != nil {
		t.Error("excuse survived Forget")
	}
}

func TestPruneDropsVanishedPaths(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mod := time.Now().Truncate(time.Second)
	s.PutProbe(&media.File{Path: "/m/kept.mkv", Size: 1, ModTime: mod})
	s.PutProbe(&media.File{Path: "/m/deleted.mkv", Size: 1, ModTime: mod})
	if err := s.Reject("/m/deleted.mkv", 1, mod, "too big"); err != nil {
		t.Fatal(err)
	}
	if err := s.Reject("/m/kept.mkv", 1, mod, "too big"); err != nil {
		t.Fatal(err)
	}

	// One cached probe and one excuse, both belonging to the vanished path.
	if n := s.Prune(map[string]bool{"/m/kept.mkv": true}); n != 2 {
		t.Errorf("pruned %d, want 2", n)
	}
	if s.Cached("/m/kept.mkv", 1, mod) == nil {
		t.Error("pruned a cached probe for a path that still exists")
	}
	if s.Excuse("/m/kept.mkv", 1, mod) == nil {
		t.Error("pruned an excuse for a path that still exists")
	}
	if s.Excuse("/m/deleted.mkv", 1, mod) != nil {
		t.Error("excuse for a vanished path survived")
	}
}
