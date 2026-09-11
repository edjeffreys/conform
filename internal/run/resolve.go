package run

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/encoder"
	"github.com/edjeffreys/conform/internal/media"
)

type choice struct {
	encoder.Candidate
	Device string
	About  string
}

func (c choice) String() string {
	s := c.Name
	switch {
	case c.Accel == encoder.NVENC:
		s += " on GPU " + c.Device
	case c.Device != "":
		s += " on " + c.Device
	}
	if c.About != "" {
		s += " (" + c.About + ")"
	}
	return s
}

type attempt struct {
	choice
	why string
}

func (r *Runner) resolve(ctx context.Context, prof config.Profile, f *media.File) (config.Profile, error) {
	e := prof.Video.Encoder
	if e.Codec == "" {
		return prof, nil
	}
	high := slices.ContainsFunc(f.Of(media.Video), media.Stream.HighBitDepth)

	c, err := r.choose(ctx, e, high)
	if err != nil {
		return prof, err
	}
	rendered := encoder.Render(c.Candidate, c.Device, e.Quality, e.Options, high)
	prof.Video.Encoder = config.Encoder{
		Name: rendered.Name, Options: rendered.Options,
		InputArgs: rendered.InputArgs, Filter: rendered.Filter,
	}
	prof.Video.ScaleFilter = rendered.ScaleFilter
	return prof, nil
}

// Cached per process: hardware does not change under a running worker.
func (r *Runner) choose(ctx context.Context, e config.Encoder, high bool) (choice, error) {
	depth := "8-bit"
	if high {
		depth = "10-bit"
	}
	key := strings.Join([]string{e.Codec, depth, e.Quality, strings.Join(e.Accel, ",")}, "|")

	r.encoderMu.Lock()
	defer r.encoderMu.Unlock()
	if c, ok := r.choices[key]; ok {
		return c.choice, c.err
	}

	var order []encoder.Accel
	var tried []attempt
	candidates := encoder.Candidates(e.Codec, nil)
	for _, a := range e.Accel {
		order = append(order, encoder.Accel(a))
		if !slices.ContainsFunc(candidates, func(c encoder.Candidate) bool { return c.Accel == encoder.Accel(a) }) {
			tried = append(tried, attempt{choice{Candidate: encoder.Candidate{Name: a}}, "has no " + e.Codec + " encoder"})
		}
	}
	c, failed, ok := pick(encoder.Candidates(e.Codec, order), renderNodes, func(c choice) (string, error) {
		return r.tryEncoder(ctx, c, e, high)
	})
	tried = append(tried, failed...)
	if ctx.Err() != nil {
		return choice{}, ctx.Err()
	}

	var err error
	switch {
	case !ok:
		err = fmt.Errorf("this worker cannot encode: no encoder for %s %s works here\n%s", depth, e.Codec, attempts(tried))
	case c.Accel == encoder.Software && len(tried) > 0:
		r.notef("encoder   %s %s → %s — SOFTWARE: no hardware encoder for it works on this worker\n%s",
			depth, e.Codec, c, attempts(tried))
	case c.Accel == encoder.Software:
		r.notef("encoder   %s %s → %s — SOFTWARE, as the profile's accel asks", depth, e.Codec, c)
	default:
		r.notef("encoder   %s %s → %s", depth, e.Codec, c)
		if len(tried) > 0 {
			r.logf("passed over for %s %s:\n%s", depth, e.Codec, attempts(tried))
		}
	}

	if r.choices == nil {
		r.choices = map[string]chosen{}
	}
	r.choices[key] = chosen{c, err}
	return c, err
}

type chosen struct {
	choice
	err error
}

func pick(candidates []encoder.Candidate, devices func(encoder.Accel) ([]string, string), try func(choice) (string, error)) (choice, []attempt, bool) {
	var tried []attempt
	for _, cand := range candidates {
		nodes, none := devices(cand.Accel)
		if len(nodes) == 0 {
			tried = append(tried, attempt{choice{Candidate: cand}, none})
			continue
		}
		for _, dev := range nodes {
			c := choice{Candidate: cand, Device: dev}
			about, err := try(c)
			if err == nil {
				c.About = about
				return c, tried, true
			}
			tried = append(tried, attempt{c, err.Error()})
		}
	}
	return choice{}, tried, false
}

func attempts(tried []attempt) string {
	var b strings.Builder
	for _, a := range tried {
		fmt.Fprintf(&b, "          ✗ %s: %s\n", a.choice, a.why)
	}
	return strings.TrimRight(b.String(), "\n")
}

func renderNodes(a encoder.Accel) ([]string, string) {
	switch a {
	case encoder.VAAPI, encoder.QSV:
		nodes, _ := filepath.Glob("/dev/dri/renderD*")
		return nodes, "no /dev/dri/renderD* device in this container"
	case encoder.NVENC:
		return []string{"0"}, ""
	}
	return []string{""}, ""
}

// Probed afterwards: some encoders accept 10-bit frames and quietly write
// 8-bit, so exiting 0 proves nothing about depth.
func (r *Runner) tryEncoder(ctx context.Context, c choice, e config.Encoder, high bool) (string, error) {
	if !r.inBuild(ctx, c.Name) {
		return "", fmt.Errorf("not in this ffmpeg build")
	}

	dir := r.Exec.TempDir
	if dir == "" {
		dir = os.TempDir()
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	out := filepath.Join(dir, fmt.Sprintf("conform-detect-%s-%d.mkv", c.Name, os.Getpid()))
	defer os.Remove(out)

	rendered := encoder.Render(c.Candidate, c.Device, e.Quality, e.Options, high)
	source := "nv12"
	if high {
		source = "p010le"
	}
	filter := "format=" + source
	if rendered.Filter != "" {
		filter += "," + rendered.Filter
	}

	args := append([]string{"-hide_banner", "-nostdin", "-y", "-v", "verbose"}, rendered.InputArgs...)
	args = append(args, "-f", "lavfi", "-i", "testsrc2=s=1280x720:r=25:d=0.2", "-vf", filter, "-c:v", rendered.Name)
	for _, k := range slices.Sorted(maps.Keys(rendered.Options)) {
		args = append(args, "-"+k, rendered.Options[k])
	}
	args = append(args, out)

	// The device description comes early in verbose output, past runFFmpeg's tail.
	var full strings.Builder
	if _, err := runFFmpeg(ctx, r.Exec.FFmpeg, args, &full); err != nil {
		return "", fmt.Errorf("%s", lastError(full.String()))
	}
	stderr := full.String()
	probed, err := r.Prober.Probe(ctx, out)
	if err != nil {
		return "", err
	}
	if v := probed.Of(media.Video); len(v) == 0 || v[0].HighBitDepth() != high {
		return "", fmt.Errorf("wrote %s from %s frames", pixFmt(v), source)
	}
	return strings.Join(slices.DeleteFunc([]string{about(stderr), hardware(c)}, func(s string) bool { return s == "" }), "; "), nil
}

// The driver and PCI ID name the GPU behind a render node where an API's own
// description ("Intel iHD driver for Intel(R) Gen Graphics") does not.
func hardware(c choice) string {
	switch c.Accel {
	case encoder.VAAPI, encoder.QSV:
		uevent, err := os.ReadFile(filepath.Join("/sys/class/drm", filepath.Base(c.Device), "device", "uevent"))
		if err != nil {
			return ""
		}
		var driver, id string
		for _, line := range strings.Split(string(uevent), "\n") {
			if v, ok := strings.CutPrefix(line, "DRIVER="); ok {
				driver = v
			}
			if v, ok := strings.CutPrefix(line, "PCI_ID="); ok {
				id = "PCI " + strings.ToLower(v)
			}
		}
		return strings.TrimSpace(driver + " " + id)
	case encoder.VideoToolbox:
		out, _ := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output()
		return strings.TrimSpace(string(out))
	}
	return ""
}

// A listing that cannot be read admits everything; the test encode then
// reports a missing encoder itself.
func (r *Runner) inBuild(ctx context.Context, name string) bool {
	if r.encoders == nil {
		r.encoders = map[string]bool{}
		out, _ := exec.CommandContext(ctx, r.Exec.FFmpeg, "-hide_banner", "-encoders").Output()
		for _, line := range strings.Split(string(out), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && len(f[0]) == 6 {
				r.encoders[f[1]] = true
			}
		}
	}
	return len(r.encoders) == 0 || r.encoders[name]
}

var aboutPatterns = []*regexp.Regexp{
	regexp.MustCompile(`VAAPI driver: (.+?)\s*(\(\))?\.?\s*$`),
	regexp.MustCompile(`GPU #\d+ - < (.+?) >`),
	regexp.MustCompile(`MFX session using (.+) implementation`),
}

func about(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		for _, re := range aboutPatterns {
			if m := re.FindStringSubmatch(line); m != nil {
				return strings.TrimSpace(m[1])
			}
		}
	}
	return ""
}

func lastError(stderr string) string {
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.ToLower(lines[i])
		if strings.Contains(l, "error") || strings.Contains(l, "fail") || strings.Contains(l, "not ") || strings.Contains(l, "unsupported") {
			return strings.TrimSpace(lines[i])
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

func pixFmt(v []media.Stream) string {
	if len(v) == 0 || v[0].PixFmt == "" {
		return "no video"
	}
	return v[0].PixFmt
}
