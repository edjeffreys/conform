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
