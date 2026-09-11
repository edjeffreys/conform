// Package encoder knows ffmpeg's encoders but not the machine: which candidate
// works is found by the worker that encodes, so no plan depends on hardware.
package encoder

import (
	"maps"
	"slices"
	"strconv"
)

type Accel string

const (
	NVENC        Accel = "nvenc"
	QSV          Accel = "qsv"
	VAAPI        Accel = "vaapi"
	VideoToolbox Accel = "videotoolbox"
	Software     Accel = "software"
)

// Software last: reached only when no hardware encoder for the codec works.
var Order = []Accel{NVENC, QSV, VAAPI, VideoToolbox, Software}

var Levels = []string{"highest", "very-high", "high", "balanced", "low", "very-low", "lowest"}

const DefaultLevel = "balanced"

func ValidAccel(a string) bool    { return slices.Contains(Order, Accel(a)) }
func ValidLevel(l string) bool    { return slices.Contains(Levels, l) }
func HasPreset(codec string) bool { _, ok := presets[codec]; return ok }

func PresetCodecs() []string { return slices.Sorted(maps.Keys(presets)) }

type spec struct {
	codec   string
	accel   Accel
	quality func(i int) map[string]string
}

var (
	qp51  = []int{17, 19, 21, 23, 26, 28, 31}
	qp255 = []int{72, 88, 104, 120, 140, 160, 180}
	crf63 = []int{18, 22, 26, 30, 35, 40, 46}
	// VideoToolbox's scale runs the other way: higher is better.
	vtq = []int{80, 75, 70, 65, 58, 50, 42}
)

func opt(key string, table []int, extra ...string) func(int) map[string]string {
	return func(i int) map[string]string {
		m := map[string]string{key: strconv.Itoa(table[i])}
		for j := 0; j+1 < len(extra); j += 2 {
			m[extra[j]] = extra[j+1]
		}
		return m
	}
}

var specs = map[string]spec{
	"libx264":           {"h264", Software, opt("crf", []int{16, 18, 20, 22, 24, 26, 29})},
	"h264_nvenc":        {"h264", NVENC, opt("cq", qp51, "rc", "vbr", "b", "0")},
	"h264_qsv":          {"h264", QSV, opt("global_quality", qp51)},
	"h264_vaapi":        {"h264", VAAPI, opt("global_quality", qp51)},
	"h264_videotoolbox": {"h264", VideoToolbox, opt("q", vtq)},

	// x265's own banner goes to stderr and buries the error a failure reports.
	"libx265":           {"hevc", Software, opt("crf", []int{16, 18, 20, 22, 25, 28, 31}, "x265-params", "log-level=error")},
	"hevc_nvenc":        {"hevc", NVENC, opt("cq", qp51, "rc", "vbr", "b", "0")},
	"hevc_qsv":          {"hevc", QSV, opt("global_quality", qp51)},
	"hevc_vaapi":        {"hevc", VAAPI, opt("global_quality", qp51)},
	"hevc_videotoolbox": {"hevc", VideoToolbox, opt("q", vtq)},

	"libsvtav1": {"av1", Software, opt("crf", crf63)},
	"av1_nvenc": {"av1", NVENC, opt("cq", crf63, "rc", "vbr", "b", "0")},
	"av1_qsv":   {"av1", QSV, opt("global_quality", qp51)},
	"av1_vaapi": {"av1", VAAPI, opt("global_quality", qp255)},
}

// So a custom encoder writing a codec its own profile rejects fails at load.
var produces = map[string]string{
	"libaom-av1": "av1", "librav1e": "av1",
	"libvpx-vp9": "vp9", "vp9_vaapi": "vp9", "vp9_qsv": "vp9",
	"mpeg4": "mpeg4", "libxvid": "mpeg4",

	"aac": "aac", "libfdk_aac": "aac", "aac_at": "aac",
	"ac3": "ac3", "eac3": "eac3",
	"libopus": "opus", "opus": "opus",
	"flac": "flac", "libmp3lame": "mp3", "truehd": "truehd", "dca": "dts",
}

var presets = map[string][]string{}

func init() {
	for name, s := range specs {
		presets[s.codec] = append(presets[s.codec], name)
	}
}

func Produces(name string) (string, bool) {
	if s, ok := specs[name]; ok {
		return s.codec, true
	}
	c, ok := produces[name]
	return c, ok
}

type Candidate struct {
	Name  string
	Accel Accel
}

func Candidates(codec string, order []Accel) []Candidate {
	if len(order) == 0 {
		order = Order
	}
	var out []Candidate
	for _, a := range order {
		for _, name := range presets[codec] {
			if specs[name].accel == a {
				out = append(out, Candidate{Name: name, Accel: a})
			}
		}
	}
	return out
}

type Rendered struct {
	Name        string
	Options     map[string]string
	InputArgs   []string
	Filter      string
	ScaleFilter string
}

// device is a render node for VAAPI and QSV, a GPU index for NVENC.
func Render(c Candidate, device, level string, options map[string]string, highBitDepth bool) Rendered {
	i := slices.Index(Levels, level)
	if i < 0 {
		i = slices.Index(Levels, DefaultLevel)
	}
	r := Rendered{Name: c.Name, Options: specs[c.Name].quality(i)}
	maps.Copy(r.Options, options)

	// hwupload passes GPU-decoded frames through and uploads the rest, so a
	// source the GPU cannot decode still reaches the encoder.
	hw := func(kind, init, frames, scale string) {
		r.InputArgs = []string{
			"-init_hw_device", init, "-filter_hw_device", "conform",
			"-hwaccel", kind, "-hwaccel_output_format", frames, "-hwaccel_device", "conform",
		}
		// Pinned to the source's depth: offered both, an upload converts 10-bit
		// down to whatever the encoder prefers, and nothing downstream notices.
		upload := "nv12"
		if highBitDepth {
			upload = "p010le"
		}
		r.Filter = "format=" + upload + "|" + frames + ",hwupload"
		r.ScaleFilter = scale + "=w={width}:h={height}"
	}
	switch c.Accel {
	case VAAPI:
		hw("vaapi", "vaapi=conform:"+device, "vaapi", "scale_vaapi")
	case QSV:
		hw("qsv", "qsv=conform:hw_any,child_device="+device, "qsv", "scale_qsv")
		r.Filter += "=extra_hw_frames=64"
	case NVENC:
		hw("cuda", "cuda=conform:"+device, "cuda", "scale_cuda")
	case VideoToolbox:
		hw("videotoolbox", "videotoolbox=conform", "videotoolbox_vld", "scale_vt")
	default:
		r.ScaleFilter = "scale={width}:{height}"
	}
	return r
}
