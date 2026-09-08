package scan

import (
	"testing"

	"github.com/edjeffreys/conform/internal/config"
)

func TestIncludes(t *testing.T) {
	lib := config.Library{
		Path:       "/media/films",
		Extensions: []string{".mkv", ".mp4"},
		Exclude:    []string{"samples/*", "*.sample.mkv", "Extras"},
	}

	cases := []struct {
		path string
		want bool
	}{
		{"/media/films/A Film/A Film.mkv", true},
		{"/media/films/A Film/A Film.MKV", true},
		{"/media/films/A Film/A Film.avi", false},
		{"/media/films/A Film/A Film", false},
		{"/media/films/.hidden.mkv", false},
		{"/media/films/samples/one.mkv", false},
		{"/media/films/A Film/A Film.sample.mkv", false},
		{"/media/films/Extras", false},
		// Only the pattern's own depth matches: filepath.Match gives * no
		// authority over a separator.
		{"/media/films/deep/samples/one.mkv", true},
	}
	for _, c := range cases {
		if got := Includes(lib, c.path); got != c.want {
			t.Errorf("Includes(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestIncludesEmptyExtensionsMatchesNothing(t *testing.T) {
	if Includes(config.Library{Path: "/media"}, "/media/a.mkv") {
		t.Error("a library with no extensions should cover nothing; config supplies the defaults")
	}
}
