package webhook

import (
	"testing"

	"github.com/edjeffreys/conform/internal/config"
)

func TestRewrite(t *testing.T) {
	rules := []config.Rewrite{
		{From: "/tv", To: "/data/TV"},
		{From: "/tv/Anime", To: "/data/Anime"},
		{From: "/movies", To: "/data/Movies"},
	}
	cases := []struct {
		path, want string
	}{
		{"/tv/Show/Season 1/e1.mkv", "/data/TV/Show/Season 1/e1.mkv"},
		{"/tv/Anime/Show/e1.mkv", "/data/Anime/Show/e1.mkv"},
		{"/tvshows/Show/e1.mkv", "/tvshows/Show/e1.mkv"},
		{"/tv", "/data/TV"},
		{"/tv//Show/./e1.mkv", "/data/TV/Show/e1.mkv"},
		{"/data/TV/Show/e1.mkv", "/data/TV/Show/e1.mkv"},
		{"/movies/Film (2026)/Film (2026).mkv", "/data/Movies/Film (2026)/Film (2026).mkv"},
	}
	for _, c := range cases {
		if got := rewrite(c.path, rules); got != c.want {
			t.Errorf("rewrite(%q) = %q, want %q", c.path, got, c.want)
		}
	}

	if got := rewrite("/downloads/a.mkv", []config.Rewrite{{From: "/", To: "/mnt"}}); got != "/mnt/downloads/a.mkv" {
		t.Errorf("a root rule rewrote to %q", got)
	}
}
