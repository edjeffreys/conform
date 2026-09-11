package main

import (
	"path/filepath"
	"testing"

	"github.com/edjeffreys/conform/internal/config"
)

func TestLibraryFor(t *testing.T) {
	root := t.TempDir()
	s := &session{cfg: &config.Config{Libraries: []config.Library{
		{Name: "films", Path: filepath.Join(root, "films")},
		{Name: "kids", Path: filepath.Join(root, "films", "kids")},
		{Name: "relative", Path: "tv"},
	}}}

	cases := []struct {
		path string
		want string
	}{
		{filepath.Join(root, "films", "a.mkv"), "films"},
		{filepath.Join(root, "films", "kids", "a.mkv"), "kids"},
		{filepath.Join(root, "elsewhere", "a.mkv"), ""},
		// A sibling whose name merely starts with the library's.
		{filepath.Join(root, "films-old", "a.mkv"), ""},
	}
	for _, c := range cases {
		lib, ok := s.libraryFor(c.path)
		switch {
		case ok != (c.want != ""):
			t.Errorf("libraryFor(%q) found = %v, want %v", c.path, ok, c.want != "")
		case ok && lib.Name != c.want:
			t.Errorf("libraryFor(%q) = %q, want %q", c.path, lib.Name, c.want)
		}
	}

	abs, err := filepath.Abs(filepath.Join("tv", "a.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	if lib, ok := s.libraryFor(abs); !ok || lib.Name != "relative" {
		t.Errorf("a library with a relative path should still match, got %q, %v", lib.Name, ok)
	}
}

func TestCovering(t *testing.T) {
	root := t.TempDir()
	exts := []string{".mkv"}
	s := &session{cfg: &config.Config{Libraries: []config.Library{
		{Name: "films", Path: filepath.Join(root, "films"), Extensions: exts},
		{Name: "kids", Path: filepath.Join(root, "films", "kids"), Extensions: exts},
		{Name: "tv", Path: filepath.Join(root, "tv"), Extensions: exts, Exclude: []string{"Extras/*"}},
	}}}

	cases := []struct {
		library string
		path    string
		want    string
	}{
		{"", filepath.Join(root, "films", "A", "A.mkv"), "films"},
		{"", filepath.Join(root, "films", "kids", "B.mkv"), "kids"},
		{"", filepath.Join(root, "films", "A", "A.nfo"), ""},
		{"", filepath.Join(root, "tv", "Extras", "x.mkv"), ""},
		{"", filepath.Join(root, "elsewhere", "x.mkv"), ""},
		// -library narrows the run, even where a nested library matches.
		{"films", filepath.Join(root, "films", "kids", "B.mkv"), ""},
		{"tv", filepath.Join(root, "films", "A", "A.mkv"), ""},
	}
	for _, c := range cases {
		s.library = c.library
		lib, _, ok := s.covering(c.path)
		switch {
		case ok != (c.want != ""):
			t.Errorf("-library %q: covering(%q) found = %v, want %v", c.library, c.path, ok, c.want != "")
		case ok && lib.Name != c.want:
			t.Errorf("-library %q: covering(%q) = %q, want %q", c.library, c.path, lib.Name, c.want)
		}
	}
}

// Sonarr posts absolute paths, while a full pass over a relative library
// produces relative ones; the cache and excuses must see the same string.
func TestCoveringKeysAPathTheWayAFullPassDoes(t *testing.T) {
	s := &session{cfg: &config.Config{Libraries: []config.Library{
		{Name: "local", Path: "./media", Extensions: []string{".mkv"}},
	}}}
	abs, err := filepath.Abs(filepath.Join("media", "Show", "e1.mkv"))
	if err != nil {
		t.Fatal(err)
	}
	_, got, ok := s.covering(abs)
	if want := filepath.Join("media", "Show", "e1.mkv"); !ok || got != want {
		t.Errorf("covering(%q) = %q, %v; want %q", abs, got, ok, want)
	}
}

func TestOnlyAFullRunStaysRunning(t *testing.T) {
	cases := []struct {
		name   string
		s      session
		dryRun bool
		want   bool
	}{
		{"full run", session{}, false, false},
		{"worker handed a path", session{paths: []string{"/data/TV/a.mkv"}}, false, true},
		{"limited trial", session{limit: 1}, false, true},
		{"dry run", session{}, true, true},
	}
	for _, c := range cases {
		if got := c.s.oneOff(c.dryRun); got != c.want {
			t.Errorf("%s: oneOff = %v, want %v", c.name, got, c.want)
		}
	}
}
