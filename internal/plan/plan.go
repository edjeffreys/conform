// Package plan diffs a file's observed state against a profile and decides
// what, if anything, has to change. It performs no I/O: the same inputs
// always produce the same plan, which is what makes `conform plan` a
// trustworthy preview of `conform apply`.
package plan

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/media"
)

type Action string

const (
	ActionNone Action = "none"
	// ActionRemux rewrites container and stream selection without
	// re-encoding — seconds per file, no GPU time.
	ActionRemux     Action = "remux"
	ActionTranscode Action = "transcode"
)

// Copy is the sentinel codec meaning "pass this stream through untouched".
const Copy = "copy"

type Plan struct {
	File      *media.File
	Action    Action
	Container string
	// InputArgs are hardware-decode flags that must precede -i. They are set
	// only when a video re-encode is actually planned, so a remux never pays
	// the cost of spinning up a hardware context it will not use.
	InputArgs  []string
	OutputArgs []string
	Streams    []StreamPlan
	Dropped    []Dropped
	Reasons    []string
}

type StreamPlan struct {
	Source int
	Type   string
	// Codec is Copy or an encoder name.
	Codec   string
	Options map[string]string
	Filter  string
	Reason  string

	// Language and Channels describe the stream this plan will produce, not
	// the one it reads. Ordering compares these, so a derived stream sorts on
	// what it will be — the same values the next pass will observe.
	Language string
	Channels int
	// Metadata is written onto the output stream. The language of a derived
	// stream is set here rather than left to ffmpeg's own copying, because
	// matching it back to its source on the next pass is what converges.
	Metadata map[string]string
	// Derived marks a stream the profile asked for that the source does not
	// have.
	Derived bool
}

type Dropped struct {
	Source int
	Type   string
	Reason string
}

// AddsStreams reports whether the plan produces a stream the source lacks,
// which is what makes its output legitimately larger than its input.
func (p *Plan) AddsStreams() bool {
	for _, s := range p.Streams {
		if s.Derived {
			return true
		}
	}
	return false
}

func (p *Plan) transcodes() bool {
	for _, s := range p.Streams {
		if s.Codec != Copy {
			return true
		}
	}
	return false
}

func Build(f *media.File, prof config.Profile) *Plan {
	p := &Plan{File: f, Container: prof.Container, OutputArgs: prof.OutputArgs}

	// Cover art is a video stream to ffprobe. Judging it as one would mark
	// every file with a poster as needing a re-encode of one still frame.
	var realVideo []media.Stream
	for _, s := range f.Streams {
		if s.Type == media.Video && s.AttachedPic {
			p.Streams = append(p.Streams, StreamPlan{Source: s.Index, Type: s.Type, Codec: Copy,
				Language: s.Language, Reason: "cover art"})
			continue
		}
		if s.Type == media.Video {
			realVideo = append(realVideo, s)
		}
	}

	for _, s := range realVideo {
		p.Streams = append(p.Streams, planVideo(s, prof.Video, p))
	}
	p.planAudio(f.Of(media.Audio), prof.Audio)
	p.planSubtitles(f.Of(media.Subtitle), prof.Subtitles)

	// The passes above append out of order; output stream order should match
	// input order.
	slices.SortStableFunc(p.Streams, func(a, b StreamPlan) int { return a.Source - b.Source })

	reordered := p.reorder(media.Audio, prof.Audio.Order, prof.Audio.Languages)
	if reordered {
		p.Reasons = append(p.Reasons, "audio: streams are not in the profile's order")
	}
	if p.reorder(media.Subtitle, prof.Subtitles.Order, prof.Subtitles.Languages) {
		p.Reasons = append(p.Reasons, "subtitles: streams are not in the profile's order")
		reordered = true
	}

	if f.Container != prof.Container {
		p.Reasons = append(p.Reasons, fmt.Sprintf("container %s is not %s", f.Container, prof.Container))
	}

	switch {
	case len(realVideo) == 0:
		// Rewriting on container grounds alone would be a change with no
		// benefit.
		p.Action = ActionNone
		p.Reasons = []string{"no video stream"}
	case p.transcodes():
		p.Action = ActionTranscode
	case f.Container != prof.Container || len(p.Dropped) > 0 || reordered:
		p.Action = ActionRemux
	default:
		p.Action = ActionNone
	}
	return p
}

func planVideo(s media.Stream, rules config.VideoRules, p *Plan) StreamPlan {
	sp := StreamPlan{Source: s.Index, Type: s.Type, Codec: Copy, Language: s.Language}

	var why []string
	if len(rules.Codecs) > 0 && !slices.Contains(rules.Codecs, s.Codec) {
		why = append(why, fmt.Sprintf("codec %s not in %s", s.Codec, strings.Join(rules.Codecs, "/")))
	}
	downscale := rules.MaxHeight > 0 && s.Height > rules.MaxHeight
	if downscale {
		why = append(why, fmt.Sprintf("height %d exceeds %d", s.Height, rules.MaxHeight))
	}
	if len(why) == 0 {
		return sp
	}

	sp.Codec = rules.Encoder.Name
	sp.Options = copyOptions(rules.Encoder.Options)
	sp.Reason = strings.Join(why, ", ")
	p.InputArgs = rules.Encoder.InputArgs
	if downscale {
		sp.Filter = strings.NewReplacer(
			"{height}", strconv.Itoa(rules.MaxHeight),
			"{width}", "-2",
		).Replace(rules.ScaleFilter)
	}
	p.Reasons = append(p.Reasons, "video: "+sp.Reason)
	return sp
}

func (p *Plan) planAudio(streams []media.Stream, rules config.AudioRules) {
	// A language filter matching nothing is ignored rather than obeyed: a
	// mis-tagged file must not be stripped of its only audio.
	keep := make([]bool, len(streams))
	kept := 0
	for i, s := range streams {
		keep[i] = len(rules.Languages) == 0 || slices.Contains(rules.Languages, s.Language)
		if keep[i] {
			kept++
		}
	}
	if kept == 0 && len(streams) > 0 {
		for i := range keep {
			keep[i] = true
		}
		p.Reasons = append(p.Reasons, "audio: language filter matched no stream, keeping all")
	}

	for i, s := range streams {
		if !keep[i] {
			p.Dropped = append(p.Dropped, Dropped{Source: s.Index, Type: s.Type,
				Reason: fmt.Sprintf("language %s not kept", s.Language)})
			continue
		}

		sp := StreamPlan{Source: s.Index, Type: s.Type, Codec: Copy,
			Language: s.Language, Channels: s.Channels}
		var why []string
		if len(rules.Codecs) > 0 && !slices.Contains(rules.Codecs, s.Codec) {
			why = append(why, fmt.Sprintf("codec %s not in %s", s.Codec, strings.Join(rules.Codecs, "/")))
		}
		downmix := rules.MaxChannels > 0 && s.Channels > rules.MaxChannels
		if downmix {
			why = append(why, fmt.Sprintf("%d channels exceeds %d", s.Channels, rules.MaxChannels))
		}
		if len(why) > 0 {
			sp.Codec = rules.Encoder.Name
			sp.Options = copyOptions(rules.Encoder.Options)
			sp.Reason = strings.Join(why, ", ")
			if downmix {
				sp.Options["ac"] = strconv.Itoa(rules.MaxChannels)
				sp.Channels = rules.MaxChannels
			}
			p.Reasons = append(p.Reasons, fmt.Sprintf("audio:%d: %s", s.Index, sp.Reason))
		}
		p.Streams = append(p.Streams, sp)
	}
	p.addCompanions(rules)
}

// The predicate is on what the output will hold, not on what the input holds:
// a surround stream the profile is already downmixing to stereo needs no
// companion, and a file that has both is left alone. That is what makes adding
// one converge rather than repeat.
func (p *Plan) addCompanions(rules config.AudioRules) {
	sc := rules.StereoCompanion
	if sc == nil {
		return
	}

	stereo := map[string]bool{}
	for _, sp := range p.Streams {
		if sp.Type == media.Audio && sp.Channels <= 2 {
			stereo[sp.Language] = true
		}
	}

	var add []StreamPlan
	for _, sp := range p.Streams {
		if sp.Type != media.Audio || sp.Channels <= 2 || stereo[sp.Language] {
			continue
		}
		// One companion answers every surround stream sharing its language.
		stereo[sp.Language] = true
		add = append(add, StreamPlan{
			Source: sp.Source, Type: media.Audio, Codec: sc.Encoder.Name,
			Options: copyOptions(sc.Encoder.Options), Filter: sc.Filter,
			Language: sp.Language, Channels: 2, Derived: true,
			Metadata: map[string]string{"language": sp.Language, "title": sc.Title},
			Reason:   "stereo companion",
		})
		p.Reasons = append(p.Reasons,
			fmt.Sprintf("audio:%d: %d channels with no stereo companion in %s", sp.Source, sp.Channels, sp.Language))
	}
	p.Streams = append(p.Streams, add...)
}

func (p *Plan) planSubtitles(streams []media.Stream, rules config.SubtitleRules) {
	for _, s := range streams {
		switch {
		case len(rules.Languages) > 0 && !slices.Contains(rules.Languages, s.Language):
			p.Dropped = append(p.Dropped, Dropped{Source: s.Index, Type: s.Type,
				Reason: fmt.Sprintf("language %s not kept", s.Language)})
		case len(rules.Codecs) > 0 && !slices.Contains(rules.Codecs, s.Codec):
			// Dropped, never converted: image-based to text needs OCR, and
			// guessing corrupts subtitles silently rather than failing.
			p.Dropped = append(p.Dropped, Dropped{Source: s.Index, Type: s.Type,
				Reason: fmt.Sprintf("codec %s not in %s", s.Codec, strings.Join(rules.Codecs, "/"))})
		default:
			p.Streams = append(p.Streams, StreamPlan{Source: s.Index, Type: s.Type, Codec: Copy,
				Language: s.Language})
		}
	}
}

// reorder rewrites the kept streams of one type into the order the profile
// asks for, leaving every position the other types occupy untouched — so
// ordering audio never regroups a file whose types are interleaved. It reports
// whether the order changed, which is the whole of the "is this file already
// acceptable?" test.
func (p *Plan) reorder(kind string, keys, langs []string) bool {
	if len(keys) == 0 {
		return false
	}
	var at []int
	for i, sp := range p.Streams {
		if sp.Type == kind {
			at = append(at, i)
		}
	}
	if len(at) < 2 {
		return false
	}

	was := make([]StreamPlan, len(at))
	for i, pos := range at {
		was[i] = p.Streams[pos]
	}
	now := slices.Clone(was)
	slices.SortStableFunc(now, func(a, b StreamPlan) int { return compare(a, b, keys, langs) })

	changed := false
	for i, pos := range at {
		if now[i].Source != was[i].Source {
			changed = true
		}
		p.Streams[pos] = now[i]
	}
	return changed
}

// The source index breaks every tie, which is what makes the order total.
// Without it "is this file in order?" and "what order would I emit?" could
// disagree, and the file would be remuxed on every pass forever. A derived
// stream ties with the stream it came from and stays just after it.
func compare(a, b StreamPlan, keys, langs []string) int {
	for _, k := range keys {
		var c int
		switch k {
		case config.OrderLanguage:
			c = languageRank(a.Language, langs) - languageRank(b.Language, langs)
		case config.OrderChannels:
			c = b.Channels - a.Channels
		}
		if c != 0 {
			return c
		}
	}
	return a.Source - b.Source
}

// A language the profile does not list sorts last. That only arises when the
// filter matched nothing and every stream was kept.
func languageRank(lang string, langs []string) int {
	if i := slices.Index(langs, lang); i >= 0 {
		return i
	}
	return len(langs)
}

func copyOptions(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+1)
	for k, v := range in {
		out[k] = v
	}
	return out
}
