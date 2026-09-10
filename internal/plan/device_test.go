package plan

import (
	"testing"

	"github.com/edjeffreys/conform/internal/media"
)

func videoPlan(codec string, inputArgs ...string) *Plan {
	return &Plan{
		InputArgs: inputArgs,
		Streams:   []StreamPlan{{Type: media.Video, Codec: codec}},
	}
}

func TestHWDevice(t *testing.T) {
	cases := []struct {
		name string
		p    *Plan
		kind string
		spec string
	}{
		{"software encoder needs no device", videoPlan("libx265"), "", ""},
		{"a copied stream needs no device",
			&Plan{Streams: []StreamPlan{{Type: media.Video, Codec: Copy}}}, "", ""},
		{"an audio re-encode is not a video device",
			&Plan{Streams: []StreamPlan{{Type: media.Audio, Codec: "eac3"}}}, "", ""},
		{"qsv", videoPlan("hevc_qsv", "-hwaccel", "qsv"), "qsv", "qsv=conform"},
		{"vaapi", videoPlan("hevc_vaapi"), "vaapi", "vaapi=conform"},
		{"nvenc asks for a cuda device", videoPlan("hevc_nvenc"), "cuda", "cuda=conform"},
		{"an unrecognised suffix is not guessed at", videoPlan("hevc_rkmpp"), "", ""},
		{"the profile's own device node is used",
			videoPlan("hevc_vaapi", "-hwaccel_device", "/dev/dri/renderD128"),
			"vaapi", "vaapi=conform:/dev/dri/renderD128"},
		{"the profile's own device spec is reused verbatim",
			videoPlan("hevc_qsv", "-init_hw_device", "qsv=hw:/dev/dri/renderD129"),
			"qsv", "qsv=hw:/dev/dri/renderD129"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			kind, spec := c.p.HWDevice()
			if kind != c.kind || spec != c.spec {
				t.Errorf("HWDevice = %q, %q, want %q, %q", kind, spec, c.kind, c.spec)
			}
		})
	}
}
