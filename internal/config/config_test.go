package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T, body string) (*Config, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "conform.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

const valid = `
libraries:
  - name: movies
    path: /data/Movies
    profile: standard
profiles:
  standard:
    container: MKV
    video:
      codecs: [HEVC]
      maxHeight: 1080
      encoder: {name: libx265}
`

func TestLoadNormalises(t *testing.T) {
	c, err := load(t, valid)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Profiles["standard"]
	if p.Container != "mkv" || p.Video.Codecs[0] != "hevc" {
		t.Errorf("names not lowercased: %+v", p)
	}
	if p.Video.ScaleFilter == "" {
		t.Error("scaleFilter default not applied")
	}
	if got := c.Libraries[0].Extensions; len(got) == 0 || got[0] != ".mkv" {
		t.Errorf("extension defaults not applied: %v", got)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := map[string]string{
		"undefined profile": strings.Replace(valid, "profile: standard", "profile: nope", 1),
		// Constraining video with no encoder plans a transcode it cannot emit.
		"constraint with no encoder": strings.Replace(valid, "      encoder: {name: libx265}\n", "", 1),
		// A mistyped rule name would silently disable the rule.
		"unknown field": strings.Replace(valid, "maxHeight", "max_height", 1),
		"no libraries":  "profiles:\n  standard:\n    container: mkv\n",
		// An order key that does not exist would otherwise sort by nothing and
		// silently leave the file in whatever order it arrived in.
		"unknown order key": valid + "    audio:\n      order: [bitrate]\n",
		// Subtitle streams have no channel count to order by.
		"channels on subtitles": valid + "    subtitles:\n      order: [channels]\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, body); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}
