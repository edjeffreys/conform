package config

import (
	"fmt"
	"slices"
)

// Catches an encoder writing a codec its own profile rejects, which would
// re-encode every file and refuse every output. Unlisted encoders are allowed.
var produces = map[string]string{
	"libx264":           "h264",
	"h264_amf":          "h264",
	"h264_nvenc":        "h264",
	"h264_qsv":          "h264",
	"h264_vaapi":        "h264",
	"h264_videotoolbox": "h264",

	"libx265":           "hevc",
	"hevc_amf":          "hevc",
	"hevc_nvenc":        "hevc",
	"hevc_qsv":          "hevc",
	"hevc_vaapi":        "hevc",
	"hevc_videotoolbox": "hevc",

	"libaom-av1": "av1",
	"librav1e":   "av1",
	"libsvtav1":  "av1",
	"av1_amf":    "av1",
	"av1_nvenc":  "av1",
	"av1_qsv":    "av1",
	"av1_vaapi":  "av1",

	"libvpx-vp9": "vp9",
	"vp9_qsv":    "vp9",
	"vp9_vaapi":  "vp9",
	"libxvid":    "mpeg4",
	"mpeg4":      "mpeg4",

	"aac":        "aac",
	"aac_at":     "aac",
	"libfdk_aac": "aac",
	"ac3":        "ac3",
	"eac3":       "eac3",
	"dca":        "dts",
	"flac":       "flac",
	"libmp3lame": "mp3",
	"libopus":    "opus",
	"opus":       "opus",
	"truehd":     "truehd",
}

func producesAcceptable(name string, codecs []string) error {
	c, ok := produces[name]
	if !ok || len(codecs) == 0 || slices.Contains(codecs, c) {
		return nil
	}
	return fmt.Errorf("encoder %s writes %s, which codecs %v does not accept", name, c, codecs)
}
