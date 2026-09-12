package config

import (
	"fmt"
	"slices"
)

// Catches an encoder writing a codec its own profile rejects, which would
// re-encode every file and refuse every output. Unlisted encoders are allowed.
var produces = map[string]string{
	"libx264": "h264", "h264_nvenc": "h264", "h264_qsv": "h264", "h264_vaapi": "h264",
	"h264_videotoolbox": "h264", "h264_amf": "h264",

	"libx265": "hevc", "hevc_nvenc": "hevc", "hevc_qsv": "hevc", "hevc_vaapi": "hevc",
	"hevc_videotoolbox": "hevc", "hevc_amf": "hevc",

	"libsvtav1": "av1", "libaom-av1": "av1", "librav1e": "av1", "av1_nvenc": "av1",
	"av1_qsv": "av1", "av1_vaapi": "av1", "av1_amf": "av1",

	"libvpx-vp9": "vp9", "vp9_vaapi": "vp9", "vp9_qsv": "vp9",
	"mpeg4": "mpeg4", "libxvid": "mpeg4",

	"aac": "aac", "libfdk_aac": "aac", "aac_at": "aac",
	"ac3": "ac3", "eac3": "eac3",
	"libopus": "opus", "opus": "opus",
	"flac": "flac", "libmp3lame": "mp3", "truehd": "truehd", "dca": "dts",
}

func producesAcceptable(name string, codecs []string) error {
	c, ok := produces[name]
	if !ok || len(codecs) == 0 || slices.Contains(codecs, c) {
		return nil
	}
	return fmt.Errorf("encoder %q writes %s, which is not in codecs %v, so its output would never conform", name, c, codecs)
}
