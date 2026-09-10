package plan

import (
	"strings"

	"github.com/edjeffreys/conform/internal/media"
)

// The device type behind an encoder's suffix. Only types whose device can be
// created on its own are listed: an encoder that is not here is reported as
// needing none, rather than as needing a guess.
var encoderDevice = map[string]string{
	"qsv":          "qsv",
	"vaapi":        "vaapi",
	"nvenc":        "cuda",
	"videotoolbox": "videotoolbox",
}

// HWDevice reports the hardware this plan's video encoder needs: its type, and
// the -init_hw_device argument that creates it. The argument names the same
// device the encode itself would open, so creating it is proof the encode can
// start. Both are empty for a plan that needs no device, or one naming
// hardware conform has no honest way to check.
func (p *Plan) HWDevice() (kind, spec string) {
	enc := p.videoEncoder()
	i := strings.LastIndex(enc, "_")
	if i < 0 {
		return "", ""
	}
	kind = encoderDevice[enc[i+1:]]
	if kind == "" {
		return "", ""
	}
	// A profile that sets the device itself is reused verbatim: checking a
	// different one would prove nothing about the encode.
	if s := flagValue(p.InputArgs, "-init_hw_device"); strings.HasPrefix(s, kind+"=") {
		return kind, s
	}
	if node := flagValue(p.InputArgs, "-hwaccel_device"); node != "" {
		return kind, kind + "=conform:" + node
	}
	return kind, kind + "=conform"
}

func (p *Plan) videoEncoder() string {
	for _, s := range p.Streams {
		if s.Type == media.Video && s.Codec != Copy {
			return s.Codec
		}
	}
	return ""
}

func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
