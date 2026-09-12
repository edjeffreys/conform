package plan

import (
	"fmt"
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

// Without this, a poster reads as a second video stream and the whole file is
// re-encoded to make a 400x225 still conform.
func TestCoverArtIsRecognisedWithoutTheDisposition(t *testing.T) {
	for _, codec := range []string{"png", "mjpeg", "gif", "bmp", "webp"} {
		t.Run(codec, func(t *testing.T) {
			art := media.Stream{Type: media.Video, Codec: codec, Height: 225}
			f := file("mkv", vid("hevc", 1080), art, aud("aac", "eng", 2))
			p := Build(f, profile())
			if p.Action != ActionNone {
				t.Fatalf("got %s, want none: %s", p.Action, p)
			}
			if !slices.ContainsFunc(p.Streams, func(s StreamPlan) bool { return s.Source == 1 && s.Codec == Copy }) {
				t.Error("cover art was not carried through")
			}
		})
	}
}

// Matching on codec must not swallow the real video sitting alongside the art.
func TestARealVideoStreamIsStillJudged(t *testing.T) {
	art := media.Stream{Type: media.Video, Codec: "png", Height: 225}
	f := file("mkv", vid("h264", 1080), art, aud("aac", "eng", 2))
	p := Build(f, profile())
	if p.Action != ActionTranscode {
		t.Errorf("got %s, want transcode: h264 is still not hevc", p.Action)
	}
}

func keeping() config.Profile {
	p := ordered([]string{config.OrderLanguage}, []string{config.OrderLanguage})
	p.Audio.Languages = []string{"eng"}
	p.Subtitles.Languages = []string{"eng"}
	p.Audio.Unlisted = config.UnlistedKeep
	p.Subtitles.Unlisted = config.UnlistedKeep
	return p
}

func TestUnlistedKeepDropsNothingAndSortsListedFirst(t *testing.T) {
	f := file("mkv", vid("hevc", 1080),
		aud("aac", "fre", 2), aud("aac", "eng", 2),
		sub("subrip", "spa"), sub("subrip", "eng"))

	p := Build(f, keeping())
	if len(p.Dropped) != 0 {
		t.Errorf("dropped %v; unlisted keep must drop nothing", p.Dropped)
	}
	if got := sources(p, media.Audio); !slices.Equal(got, []int{2, 1}) {
		t.Errorf("audio order = %v, want eng before fre", got)
	}
	if got := sources(p, media.Subtitle); !slices.Equal(got, []int{4, 3}) {
		t.Errorf("subtitle order = %v, want eng before spa", got)
	}
	if p.Action != ActionRemux {
		t.Errorf("action = %s, want remux", p.Action)
	}
}

// A rule that keeps and reorders has to settle, or every pass rewrites the
// same file forever.
func TestUnlistedKeepConverges(t *testing.T) {
	after := file("mkv", vid("hevc", 1080),
		aud("aac", "eng", 2), aud("aac", "fre", 2),
		sub("subrip", "eng"), sub("subrip", "spa"))

	if got := Build(after, keeping()).Action; got != ActionNone {
		t.Errorf("action = %s, want none — this would loop", got)
	}
}

func TestUnlistedDefaultsToDropping(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("aac", "eng", 2), aud("aac", "fre", 2))

	p := Build(f, profile())
	if len(p.Dropped) != 1 || p.Dropped[0].Source != 2 {
		t.Errorf("dropped %v, want the unlisted french stream", p.Dropped)
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

func withCompanion(p config.Profile) config.Profile {
	p.Audio.StereoCompanion = &config.StereoCompanion{
		Filter:  config.DefaultCompanionFilter,
		Title:   "Stereo",
		Encoder: config.Encoder{Name: "aac", Options: map[string]string{"b": "192k"}},
	}
	return p
}

func derived(p *Plan) []StreamPlan {
	var out []StreamPlan
	for _, s := range p.Streams {
		if s.Derived {
			out = append(out, s)
		}
	}
	return out
}

func TestStereoCompanion(t *testing.T) {
	wide := profile()
	wide.Audio.MaxChannels = 0

	tests := []struct {
		name string
		file *media.File
		prof config.Profile
		want int
	}{
		{
			name: "surround with no stereo gets one",
			file: file("mkv", vid("hevc", 1080), aud("aac", "eng", 6)),
			prof: withCompanion(profile()),
			want: 1,
		},
		{
			name: "a stereo track in that language already answers it",
			file: file("mkv", vid("hevc", 1080), aud("aac", "eng", 6), aud("aac", "eng", 2)),
			prof: withCompanion(profile()),
			want: 0,
		},
		{
			name: "a stereo track in another language does not",
			file: file("mkv", vid("hevc", 1080), aud("aac", "eng", 6), aud("aac", "und", 2)),
			prof: withCompanion(profile()),
			want: 1,
		},
		{
			// The rule is a predicate on the output: this stream is already
			// becoming stereo, so nothing is missing from the result.
			name: "none when the profile downmixes it to stereo anyway",
			file: file("mkv", vid("hevc", 1080), aud("aac", "eng", 6)),
			prof: func() config.Profile { p := withCompanion(profile()); p.Audio.MaxChannels = 2; return p }(),
			want: 0,
		},
		{
			name: "one companion answers every surround stream of its language",
			file: file("mkv", vid("hevc", 1080), aud("aac", "eng", 8), aud("aac", "eng", 6)),
			prof: withCompanion(wide),
			want: 1,
		},
		{
			name: "two languages need two",
			file: file("mkv", vid("hevc", 1080), aud("aac", "eng", 6), aud("aac", "und", 6)),
			prof: withCompanion(profile()),
			want: 2,
		},
		{
			name: "no rule, no companion",
			file: file("mkv", vid("hevc", 1080), aud("aac", "eng", 6)),
			prof: profile(),
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Build(tc.file, tc.prof)
			if got := len(derived(p)); got != tc.want {
				t.Errorf("added %d companions, want %d (%s)", got, tc.want, p)
			}
			if p.AddsStreams() != (tc.want > 0) {
				t.Errorf("AddsStreams = %v with %d companions", p.AddsStreams(), tc.want)
			}
		})
	}
}

func TestStereoCompanionCarriesItsSourceLanguage(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("aac", "eng", 6))
	d := derived(Build(f, withCompanion(profile())))
	if len(d) != 1 {
		t.Fatalf("want one companion, got %d", len(d))
	}

	// Set explicitly rather than left to ffmpeg: matching it back to its
	// source language is what makes the next pass leave the file alone.
	if d[0].Metadata["language"] != "eng" {
		t.Errorf("language metadata = %q, want eng", d[0].Metadata["language"])
	}
	if d[0].Metadata["title"] != "Stereo" {
		t.Errorf("title = %q, want Stereo", d[0].Metadata["title"])
	}
	if d[0].Channels != 2 || d[0].Filter != config.DefaultCompanionFilter {
		t.Errorf("companion is not a stereo downmix: %+v", d[0])
	}
}

// The added stream reads the same input as the stream it came from, so the
// source is mapped twice and the filter has to be pinned to the second one.
func TestStereoCompanionFFmpegArgs(t *testing.T) {
	f := file("mkv", vid("hevc", 1080), aud("aac", "eng", 6))
	args := strings.Join(Build(f, withCompanion(profile())).FFmpegArgs("in", "out"), " ")

	for _, want := range []string{
		"-map 0:0 -map 0:1 -map 0:1",
		"-c:a:0 copy",
		"-c:a:1 aac",
		"-filter:a:1 " + config.DefaultCompanionFilter,
		"-b:a:1 192k",
		"-metadata:s:a:1 language=eng",
		"-metadata:s:a:1 title=Stereo",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("missing %q in:\n%s", want, args)
		}
	}
}

// A companion ties with the stream it came from, so ordering keeps the pair
// together instead of scattering it.
func TestStereoCompanionStaysWithItsSource(t *testing.T) {
	prof := withCompanion(ordered([]string{config.OrderLanguage, config.OrderChannels}, nil))
	f := file("mkv", vid("hevc", 1080), aud("aac", "und", 2), aud("aac", "eng", 6))

	p := Build(f, prof)
	var got []string
	for _, s := range p.Streams {
		if s.Type != media.Audio {
			continue
		}
		got = append(got, fmt.Sprintf("%s/%d", s.Language, s.Channels))
	}
	want := []string{"eng/6", "eng/2", "und/2"}
	if !slices.Equal(got, want) {
		t.Errorf("audio layout = %v, want %v", got, want)
	}
}

// The whole rule rests on this: the file it produces must satisfy it.
func TestStereoCompanionConverges(t *testing.T) {
	prof := withCompanion(ordered([]string{config.OrderLanguage, config.OrderChannels}, nil))
	before := file("mkv", vid("hevc", 1080), aud("aac", "eng", 6))

	p := Build(before, prof)
	if p.Action != ActionTranscode {
		t.Fatalf("expected a transcode, got %s", p.Action)
	}

	// What ffmpeg emits for that plan: the surround stream, then the stereo
	// companion tagged with its language.
	after := file("mkv", vid("hevc", 1080), aud("aac", "eng", 6), aud("aac", "eng", 2))
	if got := Build(after, prof); got.Action != ActionNone {
		t.Fatalf("the result still needs work (%s) — this would add a track every pass", got)
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

func TestCopyOnlyNeverEncodes(t *testing.T) {
	byLanguage := []string{config.OrderLanguage}
	profiles := map[string]config.Profile{
		"standard":  profile(),
		"companion": withCompanion(profile()),
		"ordered":   ordered([]string{config.OrderLanguage, config.OrderChannels}, byLanguage),
	}
	files := []*media.File{
		file("mkv", vid("av1", 2160), aud("truehd", "eng", 8), aud("aac", "eng", 6)),
		file("avi", vid("h264", 1080), aud("dts", "fre", 6), sub("hdmv_pgs_subtitle", "eng")),
		file("mp4", vid("hevc", 1080), aud("aac", "und", 6), aud("aac", "eng", 2)),
	}
	for name, prof := range profiles {
		for _, f := range files {
			p := Build(f, CopyOnly(prof))
			if p.Action == ActionTranscode || p.AddsStreams() || len(p.InputArgs) > 0 {
				t.Errorf("%s: a copy-only plan still encodes: %s", name, p)
			}
		}
	}
}

func TestCopyOnlyLeavesTheProfileAlone(t *testing.T) {
	prof := withCompanion(profile())
	CopyOnly(prof)
	if len(prof.Video.Codecs) == 0 || prof.Audio.MaxChannels == 0 {
		t.Errorf("CopyOnly changed the profile it was given: %+v", prof)
	}
	if prof.Audio.StereoCompanion == nil {
		t.Error("CopyOnly removed the stereo companion from the profile it was given")
	}
}

// The case that motivates it: the encode is not worth having, but the file
// still carries streams and an order the profile does not accept.
func TestCopyOnlyKeepsTheRemuxWork(t *testing.T) {
	prof := ordered([]string{config.OrderLanguage}, []string{config.OrderLanguage})
	prof.Audio.Languages = []string{"eng", "und"}
	prof.Subtitles.Languages = []string{"eng"}
	f := file("mp4",
		vid("av1", 1080),
		aud("aac", "und", 2), aud("truehd", "eng", 8), aud("dts", "fre", 6),
		sub("subrip", "fre"), sub("subrip", "eng"),
	)
	if got := Build(f, prof).Action; got != ActionTranscode {
		t.Fatalf("expected a transcode against the full profile, got %s", got)
	}

	p := Build(f, CopyOnly(prof))
	if p.Action != ActionRemux {
		t.Fatalf("got %s, want a remux", p)
	}
	for _, s := range p.Streams {
		if s.Codec != Copy {
			t.Errorf("stream %d is %s, want copy", s.Source, s.Codec)
		}
	}
	if len(p.Dropped) != 2 {
		t.Errorf("dropped %v, want the French audio and subtitle", p.Dropped)
	}
	if got := sources(p, media.Audio); !slices.Equal(got, []int{2, 1}) {
		t.Errorf("audio order = %v, want [2 1]", got)
	}
	if got := sources(p, media.Subtitle); !slices.Equal(got, []int{5}) {
		t.Errorf("subtitles = %v, want [5]", got)
	}
}

// The remux is verified by re-planning against CopyOnly, which judges the
// channels a copy kept rather than the ones a downmix would have written.
func TestCopyOnlyConverges(t *testing.T) {
	byLanguage := []string{config.OrderLanguage}
	prof := ordered([]string{config.OrderLanguage, config.OrderChannels}, byLanguage)
	prof.Audio.Languages = []string{"eng", "und"}
	before := file("mp4",
		vid("av1", 2160),
		aud("eac3", "eng", 6), aud("truehd", "eng", 8), aud("aac", "fre", 2),
		sub("subrip", "eng"),
	)
	if got := Build(before, CopyOnly(prof)).Action; got != ActionRemux {
		t.Fatalf("expected a remux, got %s", got)
	}

	// What ffmpeg emits for that plan: the 8-channel stream first, since a
	// copy keeps its channels, and the French track gone.
	after := file("mkv",
		vid("av1", 2160),
		aud("truehd", "eng", 8), aud("eac3", "eng", 6),
		sub("subrip", "eng"),
	)
	if got := Build(after, CopyOnly(prof)); got.Action != ActionNone {
		t.Fatalf("the remux still needs work (%s) — the fallback would reject its own output", got)
	}
}

func TestEncoderFilterRunsBeforeTheScale(t *testing.T) {
	upload := profile()
	upload.Video.Encoder.Filter = "format=nv12|vaapi,hwupload"
	upload.Video.ScaleFilter = "scale_vaapi=w={width}:h={height}"

	tests := []struct {
		name string
		file *media.File
		prof config.Profile
		want string
	}{
		{
			name: "scaled",
			file: file("mkv", vid("h264", 2160)),
			prof: upload,
			want: "format=nv12|vaapi,hwupload,scale_vaapi=w=-2:h=1080",
		},
		{
			name: "not scaled",
			file: file("mkv", vid("h264", 1080)),
			prof: upload,
			want: "format=nv12|vaapi,hwupload",
		},
		{
			name: "no encoder filter",
			file: file("mkv", vid("h264", 1080)),
			prof: profile(),
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Build(tc.file, tc.prof).Streams[0].Filter; got != tc.want {
				t.Errorf("filter = %q, want %q", got, tc.want)
			}
		})
	}
}
