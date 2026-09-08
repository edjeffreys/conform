package plan

import (
	"slices"
	"strings"
	"testing"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/media"
)

func profile() config.Profile {
	return config.Profile{
		Container: "mkv",
		Video: config.VideoRules{
			Codecs: []string{"hevc"}, MaxHeight: 1080,
			ScaleFilter: "scale=-2:{height}",
			Encoder:     config.Encoder{Name: "libx265", Options: map[string]string{"crf": "28"}},
		},
		Audio: config.AudioRules{
			Languages: []string{"eng", "und"}, Codecs: []string{"aac", "eac3"}, MaxChannels: 6,
			Encoder: config.Encoder{Name: "eac3", Options: map[string]string{"b": "640k"}},
		},
		Subtitles: config.SubtitleRules{Languages: []string{"eng"}, Codecs: []string{"subrip"}},
	}
}

func file(container string, streams ...media.Stream) *media.File {
	for i := range streams {
		streams[i].Index = i
	}
	return &media.File{Path: "/x.mkv", Container: container, Duration: 60, Size: 1000, Streams: streams}
}

func vid(codec string, height int) media.Stream {
	return media.Stream{Type: media.Video, Codec: codec, Height: height, Width: height * 16 / 9, Language: "und"}
}
func aud(codec, lang string, ch int) media.Stream {
	return media.Stream{Type: media.Audio, Codec: codec, Language: lang, Channels: ch}
}
func sub(codec, lang string) media.Stream {
	return media.Stream{Type: media.Subtitle, Codec: codec, Language: lang}
}

func TestActions(t *testing.T) {
	tests := []struct {
		name string
		file *media.File
		want Action
	}{
		{"conformant is left alone", file("mkv", vid("hevc", 1080), aud("aac", "eng", 2)), ActionNone},
		{"wrong video codec", file("mkv", vid("h264", 1080), aud("aac", "eng", 2)), ActionTranscode},
		{"too tall", file("mkv", vid("hevc", 2160), aud("aac", "eng", 2)), ActionTranscode},
		{"wrong audio codec", file("mkv", vid("hevc", 1080), aud("truehd", "eng", 8)), ActionTranscode},
		{"container alone is a remux", file("mp4", vid("hevc", 1080), aud("aac", "eng", 2)), ActionRemux},
		{"dropping a stream alone is a remux",
			file("mkv", vid("hevc", 1080), aud("aac", "eng", 2), aud("aac", "fre", 2)), ActionRemux},
		{"a file with no video is never touched", file("mkv", aud("mp3", "eng", 2)), ActionNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Build(tc.file, profile()).Action; got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// The output of a transcode must itself be conformant, or a scheduled run
// re-encodes the same files forever.
func TestConvergence(t *testing.T) {
	nonconformant := file("avi",
		vid("h264", 2160),
		aud("truehd", "eng", 8), aud("dts", "fre", 6),
		sub("subrip", "eng"), sub("hdmv_pgs_subtitle", "eng"),
	)
	p := Build(nonconformant, profile())
	if p.Action != ActionTranscode {
		t.Fatalf("expected a transcode, got %s", p.Action)
	}

	// Model what ffmpeg would emit for this plan.
	after := file("mkv", vid("hevc", 1080), aud("eac3", "eng", 6), sub("subrip", "eng"))
	if got := Build(after, profile()).Action; got != ActionNone {
		t.Fatalf("the result of a plan still needs work (%s) — this would loop", got)
	}
}

// A language filter matching nothing must not leave a silent file.
func TestAudioLanguageFilterNeverEmptiesAFile(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("aac", "jpn", 2))
	p := Build(f, profile())
	if p.Action != ActionNone {
		t.Errorf("got %s, want none — the only audio track must be kept", p.Action)
	}
	if len(p.Dropped) != 0 {
		t.Errorf("dropped %d streams; the file would have no audio", len(p.Dropped))
	}
}

// Cover art is a video stream to ffprobe.
func TestCoverArtIsCarriedNotJudged(t *testing.T) {
	art := media.Stream{Type: media.Video, Codec: "mjpeg", Height: 1500, AttachedPic: true}
	f := file("mkv", vid("hevc", 1080), art, aud("aac", "eng", 2))
	p := Build(f, profile())
	if p.Action != ActionNone {
		t.Fatalf("got %s, want none", p.Action)
	}
	if !slices.ContainsFunc(p.Streams, func(s StreamPlan) bool { return s.Source == 1 && s.Codec == Copy }) {
		t.Error("cover art was not carried through")
	}
}

func TestDownmixSetsChannelCount(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("aac", "eng", 8))
	p := Build(f, profile())
	for _, s := range p.Streams {
		if s.Type == media.Audio && s.Options["ac"] != "6" {
			t.Errorf("ac = %q, want 6", s.Options["ac"])
		}
	}
}

func ordered(audio, subs []string) config.Profile {
	p := profile()
	p.Audio.Order = audio
	p.Subtitles.Order = subs
	p.Audio.Languages = []string{"eng", "und", "fre"}
	p.Subtitles.Languages = []string{"eng", "fre"}
	return p
}

func sources(p *Plan, kind string) []int {
	var out []int
	for _, s := range p.Streams {
		if s.Type == kind {
			out = append(out, s.Source)
		}
	}
	return out
}

func TestOrdering(t *testing.T) {
	tests := []struct {
		name    string
		file    *media.File
		prof    config.Profile
		want    Action
		wantAud []int
		wantSub []int
	}{
		{
			name:    "audio follows the language list, not the file",
			file:    file("mkv", vid("hevc", 1080), aud("aac", "und", 2), aud("aac", "eng", 2)),
			prof:    ordered([]string{config.OrderLanguage}, nil),
			want:    ActionRemux,
			wantAud: []int{2, 1},
		},
		{
			name:    "more channels first",
			file:    file("mkv", vid("hevc", 1080), aud("aac", "eng", 2), aud("aac", "eng", 6)),
			prof:    ordered([]string{config.OrderChannels}, nil),
			want:    ActionRemux,
			wantAud: []int{2, 1},
		},
		{
			name:    "language wins over channels when it comes first",
			file:    file("mkv", vid("hevc", 1080), aud("aac", "und", 6), aud("aac", "eng", 2)),
			prof:    ordered([]string{config.OrderLanguage, config.OrderChannels}, nil),
			want:    ActionRemux,
			wantAud: []int{2, 1},
		},
		{
			name:    "a file already in order is left alone",
			file:    file("mkv", vid("hevc", 1080), aud("aac", "eng", 6), aud("aac", "und", 2)),
			prof:    ordered([]string{config.OrderLanguage, config.OrderChannels}, nil),
			want:    ActionNone,
			wantAud: []int{1, 2},
		},
		{
			name:    "no order rule accepts the order the file has",
			file:    file("mkv", vid("hevc", 1080), aud("aac", "und", 2), aud("aac", "eng", 6)),
			prof:    profile(),
			want:    ActionNone,
			wantAud: []int{1, 2},
		},
		{
			name: "subtitles order independently of audio",
			file: file("mkv", vid("hevc", 1080), aud("aac", "eng", 2),
				sub("subrip", "fre"), sub("subrip", "eng")),
			prof:    ordered(nil, []string{config.OrderLanguage}),
			want:    ActionRemux,
			wantAud: []int{1},
			wantSub: []int{3, 2},
		},
		{
			name:    "ties keep the file's own order",
			file:    file("mkv", vid("hevc", 1080), aud("aac", "eng", 6), aud("aac", "eng", 6)),
			prof:    ordered([]string{config.OrderLanguage, config.OrderChannels}, nil),
			want:    ActionNone,
			wantAud: []int{1, 2},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Build(tc.file, tc.prof)
			if p.Action != tc.want {
				t.Errorf("action = %s, want %s (%s)", p.Action, tc.want, p)
			}
			if got := sources(p, media.Audio); !slices.Equal(got, tc.wantAud) {
				t.Errorf("audio order = %v, want %v", got, tc.wantAud)
			}
			if got := sources(p, media.Subtitle); !slices.Equal(got, tc.wantSub) {
				t.Errorf("subtitle order = %v, want %v", got, tc.wantSub)
			}
		})
	}
}

// Reordering one type must not regroup a file whose types are interleaved.
func TestOrderingLeavesOtherTypesWhereTheyAre(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("aac", "und", 2), sub("subrip", "eng"), aud("aac", "eng", 2))
	p := Build(f, ordered([]string{config.OrderLanguage}, nil))

	var types []string
	for _, s := range p.Streams {
		types = append(types, s.Type)
	}
	want := []string{media.Video, media.Audio, media.Subtitle, media.Audio}
	if !slices.Equal(types, want) {
		t.Errorf("type layout = %v, want %v", types, want)
	}
	if got := sources(p, media.Audio); !slices.Equal(got, []int{3, 1}) {
		t.Errorf("audio order = %v, want [3 1]", got)
	}
}

// A reorder is only real if it reaches the command line, and the codec
// specifier has to follow the emitted position rather than the source index.
func TestOrderingReachesTheFFmpegArgs(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("truehd", "und", 8), aud("aac", "eng", 2))
	args := strings.Join(Build(f, ordered([]string{config.OrderLanguage}, nil)).FFmpegArgs("in", "out"), " ")

	if !strings.Contains(args, "-map 0:0 -map 0:2 -map 0:1") {
		t.Errorf("map order does not follow the plan:\n%s", args)
	}
	// Source 1 is the truehd stream, now emitted second, so the encoder must
	// be pinned to a:1.
	if !strings.Contains(args, "-c:a:1 eac3") || strings.Contains(args, "-c:a:0 eac3") {
		t.Errorf("encoder pinned to the wrong output stream:\n%s", args)
	}
}

// An order rule must not make a file that needs a transcode order differently
// once transcoded, or the run rejects its own output.
func TestOrderingConverges(t *testing.T) {
	prof := ordered([]string{config.OrderLanguage, config.OrderChannels}, []string{config.OrderLanguage})
	before := file("avi",
		vid("h264", 2160),
		aud("aac", "fre", 2), aud("truehd", "eng", 8),
		sub("subrip", "fre"), sub("subrip", "eng"),
	)
	p := Build(before, prof)
	if p.Action != ActionTranscode {
		t.Fatalf("expected a transcode, got %s", p.Action)
	}
	if got := sources(p, media.Audio); !slices.Equal(got, []int{2, 1}) {
		t.Fatalf("audio order = %v, want [2 1]", got)
	}

	// What ffmpeg emits for that plan: the chosen order, renumbered, with the
	// 8-channel truehd downmixed to 6-channel eac3.
	after := file("mkv",
		vid("hevc", 1080),
		aud("eac3", "eng", 6), aud("aac", "fre", 2),
		sub("subrip", "eng"), sub("subrip", "fre"),
	)
	if got := Build(after, prof); got.Action != ActionNone {
		t.Fatalf("the result of the plan still needs work (%s) — this would loop", got)
	}
}

// Without a full stream specifier, a file with two video tracks has the
// profile applied to both.
func TestFFmpegArgsAreStreamSpecific(t *testing.T) {
	f := file("mkv", vid("h264", 2160), aud("truehd", "eng", 8), sub("subrip", "eng"))
	args := strings.Join(Build(f, profile()).FFmpegArgs("in.mkv", "out.mkv"), " ")

	for _, want := range []string{
		"-map 0:0", "-map 0:1", "-map 0:2",
		"-c:v:0 libx265", "-crf:v:0 28", "-filter:v:0 scale=-2:1080",
		"-c:a:0 eac3", "-ac:a:0 6", "-b:a:0 640k",
		"-c:s:0 copy", "-map_chapters 0",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in:\n%s", want, args)
		}
	}
}

// Options come from a map, so without sorting the command line would differ
// between runs.
func TestFFmpegArgsAreDeterministic(t *testing.T) {
	f := file("mkv", vid("h264", 1080), aud("truehd", "eng", 8))
	first := strings.Join(Build(f, profile()).FFmpegArgs("in", "out"), " ")
	for range 20 {
		if got := strings.Join(Build(f, profile()).FFmpegArgs("in", "out"), " "); got != first {
			t.Fatalf("argument order varies between builds:\n%s\n%s", first, got)
		}
	}
}
