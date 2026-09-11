// Package config is the desired half of the reconcile: profiles describing
// what a conformant file looks like, and the libraries they apply to.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Libraries    []Library          `yaml:"libraries"`
	Profiles     map[string]Profile `yaml:"profiles"`
	Execution    Execution          `yaml:"execution"`
	Orchestrator Orchestrator       `yaml:"orchestrator"`
	Webhook      Webhook            `yaml:"webhook"`
}

type Library struct {
	Name       string   `yaml:"name"`
	Path       string   `yaml:"path"`
	Profile    string   `yaml:"profile"`
	Extensions []string `yaml:"extensions"`
	// Exclude holds glob patterns matched against the path relative to Path.
	Exclude []string `yaml:"exclude"`
	// Watch has a full apply or orchestrate stay running and act on files as
	// they arrive. Leave it off for a network mount, which delivers no events
	// for another machine's writes.
	Watch bool `yaml:"watch"`
}

// Profile is the desired end state of a file. Every rule is a predicate on
// what is acceptable, never an instruction to act: a file already satisfying
// all of them is left alone, which is what makes repeated runs converge.
type Profile struct {
	Container string `yaml:"container"`
	// OutputArgs are appended verbatim before the output file — the escape
	// hatch for muxer flags no rule models, such as -max_muxing_queue_size.
	OutputArgs []string      `yaml:"outputArgs"`
	Video      VideoRules    `yaml:"video"`
	Audio      AudioRules    `yaml:"audio"`
	Subtitles  SubtitleRules `yaml:"subtitles"`
	Job        JobSpec       `yaml:"job"`
}

// JobSpec names the PodTemplate a distributed run copies for this profile.
// Everything about placement — device resources, node selectors, tolerations,
// the mounts that make the library visible — lives in that template, which
// conform copies without interpreting. Only needed to orchestrate.
type JobSpec struct {
	PodTemplate string `yaml:"podTemplate"`
	// Used instead when the plan re-encodes the picture, so remuxes are not
	// serialised behind it on the one node holding a device.
	VideoTemplate string `yaml:"videoTemplate"`
	// Container whose args the file path is appended to. Named rather than
	// taken positionally so a template carrying a sidecar fails loudly.
	Container string `yaml:"container"`
}

type VideoRules struct {
	// Codecs that are acceptable as-is. Empty accepts any codec.
	Codecs    []string `yaml:"codecs"`
	MaxHeight int      `yaml:"maxHeight"`
	Encoder   Encoder  `yaml:"encoder"`
	// ScaleFilter is templated with {height} and {width}. Configurable because
	// it is encoder-specific: software wants `scale`, QSV `scale_qsv`.
	ScaleFilter string `yaml:"scaleFilter"`
}

type AudioRules struct {
	// Languages to keep. Empty keeps every language.
	Languages []string `yaml:"languages"`
	// What to do with a language Languages does not list: UnlistedDrop, or
	// UnlistedKeep to demote it in the order instead. Empty means drop.
	Unlisted    string   `yaml:"unlisted"`
	Codecs      []string `yaml:"codecs"`
	MaxChannels int      `yaml:"maxChannels"`
	Encoder     Encoder  `yaml:"encoder"`
	// Order the kept streams are acceptable in, as a list of OrderKeys. Like
	// every other rule it is a predicate: a file already in this order is left
	// alone. Empty accepts the order the file already has.
	Order []string `yaml:"order"`
	// StereoCompanion requires a stereo stream alongside every surround one in
	// the same language. Also a predicate: a file that already has both is
	// left alone, which is why adding one converges. Nil imposes nothing.
	StereoCompanion *StereoCompanion `yaml:"stereoCompanion"`
}

// StereoCompanion derives a stereo track from a surround one, for players that
// downmix badly — a plain downmix leaves dialogue, which lives in the centre
// channel, quiet against music and effects.
type StereoCompanion struct {
	// Filter is the downmix, configurable because the right one is a matter of
	// taste. The default lifts the centre channel relative to the rest.
	Filter string `yaml:"filter"`
	// Title distinguishes it in a player, which would otherwise show two
	// tracks of the same language with nothing to tell them apart.
	Title   string  `yaml:"title"`
	Encoder Encoder `yaml:"encoder"`
}

// Boosts the centre channel, where dialogue sits, and pulls the rest down to
// leave headroom for it.
const DefaultCompanionFilter = "pan=stereo|FL=1.4*FC+0.5*FL+0.5*BL|FR=1.4*FC+0.5*FR+0.5*BR"

type SubtitleRules struct {
	Languages []string `yaml:"languages"`
	Unlisted  string   `yaml:"unlisted"`
	// Codecs that are acceptable. Anything else is dropped, never converted.
	Codecs []string `yaml:"codecs"`
	Order  []string `yaml:"order"`
}

// Keys accepted in an `order` rule.
//
// Deliberately narrow: a key whose value the transcode itself can change —
// codec, say — could order the output differently from the input that produced
// it, and the run would reject its own work as non-conformant. These two are
// either untouched by a copy or, in the case of a downmix, settle after one
// pass.
const (
	OrderLanguage = "language" // position in the rule's own Languages list
	OrderChannels = "channels" // most channels first; audio only

	UnlistedDrop = "drop"
	UnlistedKeep = "keep" // kept, and sorted after every listed language
)

type Encoder struct {
	Name string `yaml:"name"`
	// Options become `-key value` after the encoder selection, in sorted key
	// order so a given profile always produces byte-identical arguments.
	Options map[string]string `yaml:"options"`
	// InputArgs are placed before -i, for hardware decode setup.
	InputArgs []string `yaml:"inputArgs"`
}

type Execution struct {
	TempDir  string `yaml:"tempDir"`
	StateDir string `yaml:"stateDir"`
	Workers  int    `yaml:"workers"`

	// MinDurationRatio catches a truncated encode: ffmpeg exits 0 after
	// writing a partial file more often than it reports an error.
	MinDurationRatio float64 `yaml:"minDurationRatio"`
	// MaxSizeRatio rejects an output larger than this fraction of the source.
	// A rejected file is excused rather than retried.
	MaxSizeRatio float64 `yaml:"maxSizeRatio"`
	MaxFailures  int     `yaml:"maxFailures"`

	FFmpeg  string `yaml:"ffmpeg"`
	FFprobe string `yaml:"ffprobe"`

	// Owner chowns replaced files, for exports where a fixed uid/gid is what
	// keeps the rest of the stack able to write.
	Owner *Owner `yaml:"owner"`
}

type Owner struct {
	UID int `yaml:"uid"`
	GID int `yaml:"gid"`
}

type Orchestrator struct {
	Namespace string `yaml:"namespace"`
	// MaxActive caps the Jobs in flight. Without it one pass over a large
	// library creates an API object per non-conformant file, all at once.
	MaxActive int `yaml:"maxActive"`

	// Pointers because zero is a meaningful setting for both: delete a
	// finished Job at once, and never retry a failed one.
	TTLSecondsAfterFinished *int32 `yaml:"ttlSecondsAfterFinished"`
	BackoffLimit            *int32 `yaml:"backoffLimit"`
}

// Webhook receives the paths of new files from other services. An empty
// Listen serves nothing.
type Webhook struct {
	Listen  string    `yaml:"listen"`
	Rewrite []Rewrite `yaml:"rewrite"`
}

// Rewrite maps a path prefix as a sender sees it onto the same place as
// conform sees it, for a service that mounts the library somewhere else.
type Rewrite struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

var DefaultExtensions = []string{".mkv", ".mp4", ".avi", ".m4v", ".mov", ".wmv", ".ts", ".mpg", ".mpeg"}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true) // a typo in a rule name must not silently disable it
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	e := &c.Execution
	if e.Workers <= 0 {
		e.Workers = 1
	}
	if e.MinDurationRatio == 0 {
		e.MinDurationRatio = 0.98
	}
	if e.MaxSizeRatio == 0 {
		e.MaxSizeRatio = 1.0
	}
	if e.MaxFailures == 0 {
		e.MaxFailures = 3
	}
	if e.FFmpeg == "" {
		e.FFmpeg = "ffmpeg"
	}
	if e.FFprobe == "" {
		e.FFprobe = "ffprobe"
	}
	if e.StateDir == "" {
		e.StateDir = "."
	}

	o := &c.Orchestrator
	if o.Namespace == "" {
		o.Namespace = "default"
	}
	if o.MaxActive <= 0 {
		o.MaxActive = 8
	}
	if o.TTLSecondsAfterFinished == nil {
		o.TTLSecondsAfterFinished = ptr(int32(3600))
	}
	if o.BackoffLimit == nil {
		// Infrastructure faults are what a Job retry is for; a file that
		// cannot be encoded exits 0 and is owned by the excuse ledger.
		o.BackoffLimit = ptr(int32(2))
	}

	for i := range c.Libraries {
		if len(c.Libraries[i].Extensions) == 0 {
			c.Libraries[i].Extensions = DefaultExtensions
		}
		for j, ext := range c.Libraries[i].Extensions {
			if !strings.HasPrefix(ext, ".") {
				c.Libraries[i].Extensions[j] = "." + ext
			}
			c.Libraries[i].Extensions[j] = strings.ToLower(c.Libraries[i].Extensions[j])
		}
	}

	for i, r := range c.Webhook.Rewrite {
		if r.From != "" {
			c.Webhook.Rewrite[i].From = filepath.Clean(r.From)
		}
		if r.To != "" {
			c.Webhook.Rewrite[i].To = filepath.Clean(r.To)
		}
	}

	for name, p := range c.Profiles {
		if p.Job.Container == "" {
			p.Job.Container = "conform"
		}
		if sc := p.Audio.StereoCompanion; sc != nil {
			if sc.Filter == "" {
				sc.Filter = DefaultCompanionFilter
			}
			if sc.Title == "" {
				sc.Title = "Stereo"
			}
		}
		if p.Video.ScaleFilter == "" {
			p.Video.ScaleFilter = "scale=-2:{height}"
		}
		p.Container = strings.ToLower(p.Container)
		p.Video.Codecs = lowerAll(p.Video.Codecs)
		p.Audio.Codecs = lowerAll(p.Audio.Codecs)
		p.Audio.Languages = lowerAll(p.Audio.Languages)
		p.Subtitles.Codecs = lowerAll(p.Subtitles.Codecs)
		p.Subtitles.Languages = lowerAll(p.Subtitles.Languages)
		c.Profiles[name] = p
	}
}

func (c *Config) Validate() error {
	if len(c.Libraries) == 0 {
		return fmt.Errorf("no libraries defined")
	}
	seen := map[string]bool{}
	for _, l := range c.Libraries {
		switch {
		case l.Name == "":
			return fmt.Errorf("library with path %q has no name", l.Path)
		case seen[l.Name]:
			return fmt.Errorf("duplicate library name %q", l.Name)
		case l.Path == "":
			return fmt.Errorf("library %q has no path", l.Name)
		case l.Profile == "":
			return fmt.Errorf("library %q has no profile", l.Name)
		}
		seen[l.Name] = true

		p, ok := c.Profiles[l.Profile]
		if !ok {
			return fmt.Errorf("library %q references undefined profile %q", l.Name, l.Profile)
		}
		// Same reason as the rules above: a profile that can require a stream
		// it cannot emit would plan a transcode it cannot render.
		if sc := p.Audio.StereoCompanion; sc != nil && sc.Encoder.Name == "" {
			return fmt.Errorf("profile %q sets stereoCompanion but no encoder for it", l.Profile)
		}
		if err := validOrder(p.Audio.Order, true); err != nil {
			return fmt.Errorf("profile %q audio: %w", l.Profile, err)
		}
		if err := validOrder(p.Subtitles.Order, false); err != nil {
			return fmt.Errorf("profile %q subtitles: %w", l.Profile, err)
		}
		if err := validUnlisted(p.Audio.Unlisted); err != nil {
			return fmt.Errorf("profile %q audio: %w", l.Profile, err)
		}
		if err := validUnlisted(p.Subtitles.Unlisted); err != nil {
			return fmt.Errorf("profile %q subtitles: %w", l.Profile, err)
		}
		if p.Container == "" {
			return fmt.Errorf("profile %q has no container", l.Profile)
		}
		// A profile that can reject a stream but not re-encode it would plan a
		// transcode it cannot emit.
		if len(p.Video.Codecs) > 0 || p.Video.MaxHeight > 0 {
			if p.Video.Encoder.Name == "" {
				return fmt.Errorf("profile %q constrains video but sets no video encoder", l.Profile)
			}
		}
		if len(p.Audio.Codecs) > 0 || p.Audio.MaxChannels > 0 {
			if p.Audio.Encoder.Name == "" {
				return fmt.Errorf("profile %q constrains audio but sets no audio encoder", l.Profile)
			}
		}
	}

	from := map[string]bool{}
	for _, r := range c.Webhook.Rewrite {
		switch {
		case r.From == "" || r.To == "":
			return fmt.Errorf("webhook rewrite needs both from and to, got from %q to %q", r.From, r.To)
		case from[r.From]:
			return fmt.Errorf("webhook rewrite from %q is given twice", r.From)
		}
		from[r.From] = true
	}
	return nil
}

// Profile assumes Validate has run, which guarantees the profile exists.
func (c *Config) Profile(l Library) Profile { return c.Profiles[l.Profile] }

func validUnlisted(v string) error {
	switch v {
	case "", UnlistedDrop, UnlistedKeep:
		return nil
	}
	return fmt.Errorf("unknown unlisted %q, want %q or %q", v, UnlistedDrop, UnlistedKeep)
}

func validOrder(keys []string, audio bool) error {
	for _, k := range keys {
		switch {
		case k == OrderLanguage:
		case k == OrderChannels && audio:
		case k == OrderChannels:
			return fmt.Errorf("%q orders by a value a subtitle stream does not have", k)
		default:
			return fmt.Errorf("unknown order key %q", k)
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

func lowerAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToLower(strings.TrimSpace(s))
	}
	return out
}
