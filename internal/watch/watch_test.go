package watch

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPendingSettlesOnlyAfterQuiet(t *testing.T) {
	p := newPending(time.Minute)
	t0 := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)

	p.touch("a.mkv", t0)
	p.touch("b.mkv", t0.Add(30*time.Second))
	p.touch("a.mkv", t0.Add(45*time.Second))

	if got := p.settled(t0.Add(time.Minute)); len(got) != 0 {
		t.Errorf("settled %v a minute after the first write, but both were written since", got)
	}
	if at, _ := p.next(); !at.Equal(t0.Add(90 * time.Second)) {
		t.Errorf("next = %v, want b's deadline", at)
	}
	if got := p.settled(t0.Add(105 * time.Second)); !slices.Equal(got, []string{"a.mkv", "b.mkv"}) {
		t.Errorf("settled = %v, want both", got)
	}
	if _, ok := p.next(); ok {
		t.Error("a settled path is still pending")
	}
}

func TestPendingForgetsARemovedDirectory(t *testing.T) {
	p := newPending(time.Minute)
	now := time.Now()
	p.touch(filepath.Join("lib", "Show", "e1.mkv"), now)
	p.touch(filepath.Join("lib", "Show 2", "e1.mkv"), now)

	p.forget(filepath.Join("lib", "Show"))

	got := p.settled(now.Add(time.Hour))
	if want := []string{filepath.Join("lib", "Show 2", "e1.mkv")}; !slices.Equal(got, want) {
		t.Errorf("settled = %v, want %v; a sibling sharing the prefix must survive", got, want)
	}
}

const settle = 100 * time.Millisecond

func start(t *testing.T, root string) *Watcher {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w, err := Start(ctx, []string{root}, settle, func(path string) bool {
		return strings.HasSuffix(path, ".mkv")
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		w.Close()
	})
	return w
}

func collect(t *testing.T, w *Watcher) []string {
	t.Helper()
	var got []string
	deadline := time.After(5 * time.Second)
	for {
		select {
		case batch := <-w.Changed():
			got = append(got, batch...)
		case err := <-w.Err():
			t.Fatal(err)
		case <-time.After(5 * settle):
			if len(got) > 0 {
				slices.Sort(got)
				return got
			}
		case <-deadline:
			return got
		}
	}
}

func write(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReportsANewFileInAnExistingSubdirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "Films", "A"), 0o755); err != nil {
		t.Fatal(err)
	}
	w := start(t, root)

	path := filepath.Join(root, "Films", "A", "A.mkv")
	write(t, path)
	write(t, filepath.Join(root, "Films", "A", "A.nfo"))

	if got := collect(t, w); !slices.Equal(got, []string{path}) {
		t.Errorf("changed = %v, want only %v", got, path)
	}
}

func TestReportsTheContentsOfADirectoryMovedIn(t *testing.T) {
	root := t.TempDir()
	w := start(t, root)

	outside := filepath.Join(t.TempDir(), "Show")
	e1 := filepath.Join(outside, "S01", "e1.mkv")
	e2 := filepath.Join(outside, "S01", "e2.mkv")
	write(t, e1)
	write(t, e2)
	if err := os.Rename(outside, filepath.Join(root, "Show")); err != nil {
		t.Fatal(err)
	}

	want := []string{
		filepath.Join(root, "Show", "S01", "e1.mkv"),
		filepath.Join(root, "Show", "S01", "e2.mkv"),
	}
	if got := collect(t, w); !slices.Equal(got, want) {
		t.Errorf("changed = %v, want %v", got, want)
	}

	// The new tree must be watched as well as walked.
	e3 := filepath.Join(root, "Show", "S01", "e3.mkv")
	write(t, e3)
	if got := collect(t, w); !slices.Equal(got, []string{e3}) {
		t.Errorf("after the move, changed = %v, want %v", got, e3)
	}
}

func TestIgnoresHiddenDirectories(t *testing.T) {
	root := t.TempDir()
	w := start(t, root)

	write(t, filepath.Join(root, ".incomplete", "A.mkv"))
	visible := filepath.Join(root, "B.mkv")
	write(t, visible)

	if got := collect(t, w); !slices.Equal(got, []string{visible}) {
		t.Errorf("changed = %v, want only %v", got, visible)
	}
}

func TestAFileStillBeingWrittenIsNotReported(t *testing.T) {
	root := t.TempDir()
	w := start(t, root)

	path := filepath.Join(root, "A.mkv")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	stop := time.Now().Add(6 * settle)
	for time.Now().Before(stop) {
		if _, err := f.WriteString("chunk"); err != nil {
			t.Fatal(err)
		}
		select {
		case batch := <-w.Changed():
			t.Fatalf("reported %v while it was still being written", batch)
		case <-time.After(settle / 4):
		}
	}

	if got := collect(t, w); !slices.Equal(got, []string{path}) {
		t.Errorf("once quiet, changed = %v, want %v", got, path)
	}
}
