// Package media turns a file on disk into the observed half of the
// reconcile: a normalised description of its container and streams.
package media

import (
	"strings"
	"time"
)

// Stream types, matching ffprobe's codec_type.
const (
	Video    = "video"
	Audio    = "audio"
	Subtitle = "subtitle"
)

type File struct {
	Path      string    `json:"path"`
	Size      int64     `json:"size"`
	ModTime   time.Time `json:"modTime"`
	Container string    `json:"container"`
	Duration  float64   `json:"duration"`
	Streams   []Stream  `json:"streams"`
}

// Fields not applicable to a stream's type are left zero, so the planner
// never has to type-assert.
type Stream struct {
	Index    int    `json:"index"`
	Type     string `json:"type"`
	Codec    string `json:"codec"`
	Profile  string `json:"profile,omitempty"`
	Language string `json:"language"`
	Title    string `json:"title,omitempty"`

	Width    int   `json:"width,omitempty"`
	Height   int   `json:"height,omitempty"`
	Channels int   `json:"channels,omitempty"`
	BitRate  int64 `json:"bitRate,omitempty"`

	Default bool `json:"default,omitempty"`
	Forced  bool `json:"forced,omitempty"`

	// AttachedPic marks cover art, which ffprobe reports as a video stream.
	AttachedPic bool `json:"attachedPic,omitempty"`
}

func (f *File) Of(kind string) []Stream {
	var out []Stream
	for _, s := range f.Streams {
		if s.Type == kind {
			out = append(out, s)
		}
	}
	return out
}

// ffprobe reports one demuxer for several containers ("matroska,webm"), so
// format_name can never be compared to a config string directly.
var containerAliases = []struct {
	needle string
	name   string
}{
	{"matroska", "mkv"},
	{"mp4", "mp4"},
	{"avi", "avi"},
	{"asf", "wmv"},
	{"mpegts", "ts"},
	{"flv", "flv"},
	{"webm", "webm"},
}

func normaliseContainer(formatName string) string {
	for _, a := range containerAliases {
		for _, part := range strings.Split(formatName, ",") {
			if part == a.needle {
				return a.name
			}
		}
	}
	// The first reported demuxer rather than a guess, so an unrecognised
	// container shows up in plan output.
	if i := strings.Index(formatName, ","); i > 0 {
		return formatName[:i]
	}
	return formatName
}

func Ext(container string) string {
	return "." + container
}
