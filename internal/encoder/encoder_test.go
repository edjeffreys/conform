package encoder

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Software is the last resort a default preset falls back to, so a codec
// without a software encoder could leave a worker with nothing at all.
func TestEveryPresetHasASoftwareEncoder(t *testing.T) {
	for _, codec := range PresetCodecs() {
		if !slices.ContainsFunc(Candidates(codec, nil), func(c Candidate) bool { return c.Accel == Software }) {
			t.Errorf("%s has no software candidate", codec)
		}
	}
}

// A preset resolving to an encoder that writes some other codec would have
// every output refused as not conforming.
func TestCandidatesWriteTheirPresetsCodec(t *testing.T) {
	for _, codec := range PresetCodecs() {
		for _, c := range Candidates(codec, nil) {
			if got, _ := Produces(c.Name); got != codec {
				t.Errorf("%s is a %s candidate but writes %s", c.Name, codec, got)
			}
		}
	}
}

func TestCandidatesFollowTheGivenOrder(t *testing.T) {
	got := Candidates("hevc", []Accel{Software, VAAPI})
	want := []Candidate{{"libx265", Software}, {"hevc_vaapi", VAAPI}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := Candidates("av1", []Accel{VideoToolbox}); len(got) != 0 {
		t.Errorf("videotoolbox has no av1 encoder, got %v", got)
	}
	if got := Candidates("hevc", nil); got[len(got)-1].Accel != Software {
		t.Errorf("the default order must end in software: %v", got)
	}
}

// Each step down the levels must make the output no larger, or the names lie.
func TestQualityLevelsAreOrdered(t *testing.T) {
	for name, s := range specs {
		var prev int
		for i := range Levels {
			var v int
			for k, raw := range s.quality(i) {
				if n, err := strconv.Atoi(raw); err == nil && k != "b" {
					v = n
				}
			}
			better := v < prev
			if s.accel == VideoToolbox {
				better = v > prev
			}
			if i > 0 && (v == prev || better) {
				t.Errorf("%s: %s (%d) is not below %s (%d)", name, Levels[i], v, Levels[i-1], prev)
			}
			prev = v
		}
	}
}

func TestRender(t *testing.T) {
	vaapi := Render(Candidate{"hevc_vaapi", VAAPI}, "/dev/dri/renderD129", "high", nil, false)
	if !slices.Contains(vaapi.InputArgs, "vaapi=conform:/dev/dri/renderD129") {
		t.Errorf("vaapi does not open the device it was tested on: %v", vaapi.InputArgs)
	}
	if vaapi.Filter != "format=nv12|vaapi,hwupload" || !strings.HasPrefix(vaapi.ScaleFilter, "scale_vaapi=") {
		t.Errorf("vaapi filters = %q, %q", vaapi.Filter, vaapi.ScaleFilter)
	}
	if vaapi.Options["global_quality"] != "21" {
		t.Errorf("high on hevc_vaapi = %v", vaapi.Options)
	}

	// Offered both, an upload converts 10-bit down to whatever the encoder prefers.
	if got := Render(Candidate{"hevc_vaapi", VAAPI}, "/dev/dri/renderD128", "", nil, true).Filter; got != "format=p010le|vaapi,hwupload" {
		t.Errorf("10-bit filter = %q", got)
	}

	sw := Render(Candidate{"libx265", Software}, "", "", map[string]string{"crf": "30", "preset": "slow"}, true)
	if len(sw.InputArgs) > 0 || sw.Filter != "" || sw.ScaleFilter != "scale={width}:{height}" {
		t.Errorf("software rendered hardware setup: %+v", sw)
	}
	if sw.Options["crf"] != "30" || sw.Options["preset"] != "slow" || sw.Options["x265-params"] == "" {
		t.Errorf("profile options must override the level's and keep the rest: %v", sw.Options)
	}
}
