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
		"rewrite with no to":    valid + "webhook:\n  rewrite:\n    - from: /tv\n",
		// Two targets for one prefix leave which applies to map order.
		"rewrite given twice": valid + "webhook:\n  rewrite:\n    - {from: /tv, to: /data/TV}\n    - {from: /tv/, to: /media/TV}\n",

		// Every file would be re-encoded and every output refused.
		"encoder writes a rejected codec": strings.Replace(valid, "{name: libx265}", "{name: av1_vaapi}", 1),
		"preset writes a rejected codec":  strings.Replace(valid, "{name: libx265}", "{codec: av1}", 1),
		"companion writes a rejected codec": valid + "    audio:\n      codecs: [eac3]\n      encoder: {name: eac3}\n" +
			"      stereoCompanion:\n        encoder: {name: aac}\n",
		"preset and name":                     strings.Replace(valid, "{name: libx265}", "{name: libx265, codec: hevc}", 1),
		"preset with inputArgs":               strings.Replace(valid, "{name: libx265}", "{codec: hevc, inputArgs: [-hwaccel, vaapi]}", 1),
		"preset with scaleFilter":             strings.Replace(valid, "      encoder: {name: libx265}", "      scaleFilter: scale_vaapi=h={height}\n      encoder: {codec: hevc}", 1),
		"unknown accel":                       strings.Replace(valid, "{name: libx265}", "{codec: hevc, accel: [cuda]}", 1),
		"accel given twice":                   strings.Replace(valid, "{name: libx265}", "{codec: hevc, accel: [vaapi, vaapi]}", 1),
		"accel with no encoder for the codec": strings.Replace(strings.Replace(valid, "codecs: [HEVC]", "codecs: [av1]", 1), "{name: libx265}", "{codec: av1, accel: [videotoolbox]}", 1),
		"unknown quality":                     strings.Replace(valid, "{name: libx265}", "{codec: hevc, quality: medium}", 1),
		"accel on a named encoder":            strings.Replace(valid, "{name: libx265}", "{name: libx265, accel: [software]}", 1),
		"preset on audio":                     valid + "    audio:\n      codecs: [aac]\n      encoder: {codec: aac}\n",
		"preset with no such codec":           strings.Replace(strings.Replace(valid, "codecs: [HEVC]", "codecs: [vp8]", 1), "{name: libx265}", "{codec: vp8}", 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, body); err == nil {
				t.Error("expected an error, got none")
			}
		})
	}
}

func TestLoadNewFileTriggers(t *testing.T) {
	body := strings.Replace(valid, "    profile: standard\n", "    profile: standard\n    watch: true\n", 1) +
		"webhook:\n  listen: \":8080\"\n  rewrite:\n    - {from: /tv/, to: /data/TV}\n"
	c, err := load(t, body)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Libraries[0].Watch || c.Webhook.Listen != ":8080" {
		t.Errorf("watch = %v, webhook = %+v", c.Libraries[0].Watch, c.Webhook)
	}
	if want := (Rewrite{From: "/tv", To: "/data/TV"}); len(c.Webhook.Rewrite) != 1 || c.Webhook.Rewrite[0] != want {
		t.Errorf("rewrite = %+v, want %+v", c.Webhook.Rewrite, want)
	}
}

func TestLoadAcceptsEncoders(t *testing.T) {
	tests := map[string]string{
		"preset":                strings.Replace(valid, "{name: libx265}", "{codec: HEVC, accel: [vaapi, software], quality: very-high}", 1),
		"preset with overrides": strings.Replace(valid, "{name: libx265}", "{codec: hevc, options: {g: \"48\"}}", 1),
		// The catalogue cannot know every encoder, and must not refuse one.
		"unknown encoder name": strings.Replace(valid, "{name: libx265}", "{name: hevc_amf}", 1),
		"any codec accepted":   strings.Replace(strings.Replace(valid, "      codecs: [HEVC]\n", "", 1), "{name: libx265}", "{name: av1_vaapi}", 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := load(t, body); err != nil {
				t.Error(err)
			}
		})
	}
}

// A preset brings its own scale filter, so the software default must not be
// applied over it.
func TestPresetGetsNoDefaultScaleFilter(t *testing.T) {
	c, err := load(t, strings.Replace(valid, "{name: libx265}", "{codec: hevc}", 1))
	if err != nil {
		t.Fatal(err)
	}
	if f := c.Profiles["standard"].Video.ScaleFilter; f != "" {
		t.Errorf("scaleFilter = %q, want none", f)
	}
}
