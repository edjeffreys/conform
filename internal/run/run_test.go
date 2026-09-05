package run

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A dual-format release — Film.mkv already conformant, Film.mp4 not — must not
// end with the .mp4's transcode written over the .mkv.
func TestReplaceRefusesToOverwriteUnrelatedFile(t *testing.T) {
	dir := t.TempDir()
	bystander := filepath.Join(dir, "Film.mkv")
	src := filepath.Join(dir, "Film.mp4")
	tmp := filepath.Join(t.TempDir(), "encoded.mkv")

	write(t, bystander, "the original mkv")
	write(t, src, "the mp4")
	write(t, tmp, "the transcode")

	r := &Runner{}
	if _, err := r.replace(tmp, src, "mkv"); err == nil {
		t.Fatal("replace succeeded over an unrelated file; want an error")
	}

	if got := read(t, bystander); got != "the original mkv" {
		t.Errorf("bystander was modified: %q", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source was removed despite the failure: %v", err)
	}
}

func TestReplaceOverwritesItsOwnSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Film.mkv")
	tmp := filepath.Join(t.TempDir(), "encoded.mkv")

	write(t, src, "before")
	write(t, tmp, "after")

	r := &Runner{}
	final, err := r.replace(tmp, src, "mkv")
	if err != nil {
		t.Fatal(err)
	}
	if final != src {
		t.Errorf("final = %q, want %q", final, src)
	}
	if got := read(t, src); got != "after" {
		t.Errorf("content = %q, want %q", got, "after")
	}
}

func TestReplaceContainerChangeRemovesSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Film.avi")
	tmp := filepath.Join(t.TempDir(), "encoded.mkv")

	write(t, src, "before")
	write(t, tmp, "after")

	r := &Runner{}
	final, err := r.replace(tmp, src, "mkv")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "Film.mkv"); final != want {
		t.Fatalf("final = %q, want %q", final, want)
	}
	if got := read(t, final); got != "after" {
		t.Errorf("content = %q, want %q", got, "after")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("superseded source still present: %v", err)
	}
}

// No staging file may survive a run, successful or not — the scanner skips
// dotfiles, so a leftover would be invisible rather than merely untidy.
func TestReplaceLeavesNoStagingFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Film.avi")
	tmp := filepath.Join(t.TempDir(), "encoded.mkv")
	write(t, src, "before")
	write(t, tmp, "after")

	r := &Runner{}
	if _, err := r.replace(tmp, src, "mkv"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".mkv" || e.Name()[0] == '.' {
			t.Errorf("unexpected leftover %q", e.Name())
		}
	}
}

func TestDestinationFree(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Taken.mkv"), "x")

	cases := []struct {
		name      string
		src       string
		container string
		want      bool
	}{
		{"same container is always its own destination", "Taken.mkv", "mkv", true},
		{"container change onto an occupied name", "Taken.mp4", "mkv", false},
		{"container change onto a free name", "Other.mp4", "mkv", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, got := destinationFree(filepath.Join(dir, c.src), c.container); got != c.want {
				t.Errorf("destinationFree(%q, %q) = %v, want %v", c.src, c.container, got, c.want)
			}
		})
	}
}

// Staging names key on the whole path, not the basename, so two files that
// differ only by extension cannot stage to the same place.
func TestPathHashDistinguishesExtensions(t *testing.T) {
	if pathHash("/media/Film.mkv") == pathHash("/media/Film.mp4") {
		t.Error("paths differing only by extension hash alike")
	}
}
