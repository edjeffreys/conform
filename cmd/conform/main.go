// Command conform brings a media library into line with a declared profile.
//
// It is a reconciler, not a job queue: nothing about a file's history decides
// whether it needs work, so the plan is a pure function of the library and the
// config, and running it twice is a no-op.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/edjeffreys/conform/internal/config"
	"github.com/edjeffreys/conform/internal/encoder"
	"github.com/edjeffreys/conform/internal/media"
	"github.com/edjeffreys/conform/internal/orchestrate"
	"github.com/edjeffreys/conform/internal/plan"
	"github.com/edjeffreys/conform/internal/run"
	"github.com/edjeffreys/conform/internal/scan"
	"github.com/edjeffreys/conform/internal/state"
	"github.com/edjeffreys/conform/internal/watch"
	"github.com/edjeffreys/conform/internal/webhook"

	"sigs.k8s.io/yaml"
)

var version = "dev"

func main() {
	if err := realMain(); err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "interrupted")
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func realMain() error {
	if len(os.Args) < 2 {
		usage()
		return errors.New("no command given")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "probe":
		return cmdProbe(ctx, os.Args[2:])
	case "plan":
		return cmdPlan(ctx, os.Args[2:])
	case "apply":
		return cmdApply(ctx, os.Args[2:])
	case "orchestrate":
		return cmdOrchestrate(ctx, os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("conform", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", os.Args[1])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `conform — reconcile a media library against a declared profile

  conform probe <file>          print what ffprobe sees, as conform models it
  conform plan  [flags] [file]  show what would change; touches nothing
  conform apply [flags] [file]  make it so
  conform orchestrate [flags]   create one Kubernetes Job per file needing work

Named files are judged by the library that contains them. With none given,
every configured library is walked, and apply or orchestrate then keeps
running if any library sets watch or webhook.listen is set.

Flags for plan, apply and orchestrate:
  -config PATH    config file (default conform.yaml)
  -library NAME   restrict to one library
  -limit N        stop after N files needing work
  -verbose        log the ffmpeg command lines, or the jobs orchestrate builds

Flags for apply only:
  -retry-excused  reconsider files previously excused, encodes included
  -ffmpeg-output  stream ffmpeg's own output while it runs

Flags for apply and orchestrate:
  -dry-run        go through the motions, change nothing
  -interval D     repeat forever, waiting D between passes (e.g. 6h)

Flags for orchestrate only:
  -kubeconfig P   use this kubeconfig rather than the in-cluster account
`)
}

type session struct {
	cfg       *config.Config
	prober    *media.Prober
	store     *state.Store
	paths     []string
	limit     int
	library   string
	verbose   bool
	ffmpegOut bool
	// The probe cache has a single owner. A pass over whole libraries is that
	// owner; a worker handed individual paths runs alongside others against
	// the same cache, so it reads it and never writes it.
	ownsCache bool
}

func newSession(cfgPath, library string, paths []string, limit int, verbose bool) (*session, error) {
	if len(paths) > 0 && (library != "" || limit != 0) {
		return nil, errors.New("-library and -limit select within a library; they do not apply to named files")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	store, err := state.Open(cfg.Execution.StateDir)
	if err != nil {
		return nil, err
	}
	return &session{
		cfg: cfg, prober: media.NewProber(cfg.Execution.FFprobe), store: store,
		paths: paths, limit: limit, library: library, verbose: verbose,
		ownsCache: len(paths) == 0,
	}, nil
}

type item struct {
	lib  config.Library
	prof config.Profile
	plan *plan.Plan
	// Why the file's encode is excused, when prof is plan.CopyOnly because of it.
	excusedEncode string
}

// A file whose cached probe still matches its size and mtime costs a stat
// here; the rest cost an ffprobe.
func (s *session) collect(ctx context.Context, includeExcused bool) ([]item, *tally, error) {
	var items []item
	t := &tally{}
	seen := map[string]bool{}

collect:
	for _, lib := range s.cfg.Libraries {
		if s.library != "" && lib.Name != s.library {
			continue
		}
		entries, err := scan.Walk(lib)
		if err != nil {
			return nil, nil, fmt.Errorf("scan library %q: %w", lib.Name, err)
		}
		prof := s.cfg.Profile(lib)

		for _, e := range entries {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			seen[e.Path] = true
			t.total++

			it, err := s.consider(ctx, lib, prof, e.Path, e.Info, includeExcused, t)
			if err != nil {
				t.unreadable++
				fmt.Fprintf(os.Stderr, "  ! %s: %v\n", rel(lib, e.Path), err)
				continue
			}
			if it == nil {
				continue
			}
			items = append(items, *it)
			if s.limit > 0 && len(items) >= s.limit {
				break collect
			}
		}
	}

	// Only a full, unfiltered pass knows which paths are genuinely gone.
	if s.library == "" && s.limit == 0 {
		s.store.Prune(seen)
	}
	return items, t, s.store.Save()
}

// A nil item means the file needs no work; err means no verdict was reached at
// all, which is the caller's to interpret.
func (s *session) consider(ctx context.Context, lib config.Library, prof config.Profile, path string, info fs.FileInfo, includeExcused bool, t *tally) (*item, error) {
	size, mod := info.Size(), info.ModTime()
	var excusedEncode string
	if ex := s.store.Excuse(path, size, mod); ex != nil && ex.Excused && !includeExcused {
		if !ex.EncodeOnly {
			t.excused++
			return nil, nil
		}
		excusedEncode = ex.Reason
	}

	f := s.store.Cached(path, size, mod)
	if f == nil {
		probed, err := s.prober.Probe(ctx, path)
		if err != nil {
			return nil, err
		}
		f = probed
		if s.ownsCache {
			s.store.PutProbe(f)
		}
	}

	if excusedEncode != "" {
		prof = plan.CopyOnly(prof)
	}
	p := plan.Build(f, prof)
	if p.Action == plan.ActionNone && excusedEncode != "" {
		t.excused++
		return nil, nil
	}
	t.count(p.Action)
	if p.Action == plan.ActionNone {
		return nil, nil
	}
	return &item{lib: lib, prof: prof, plan: p, excusedEncode: excusedEncode}, nil
}

// changed is nil for a full pass, and otherwise holds the paths a watch saw.
func (s *session) selection(ctx context.Context, includeExcused bool, changed []string) ([]item, *tally, error) {
	switch {
	case changed != nil:
		return s.collectChanged(ctx, changed, includeExcused)
	case len(s.paths) > 0:
		return s.collectFiles(ctx, includeExcused)
	}
	return s.collect(ctx, includeExcused)
}

// The plan for a named file is re-derived here rather than passed in, which is
// what lets a worker reach the same answer as whatever picked the file without
// a description of the work travelling between them.
func (s *session) collectFiles(ctx context.Context, includeExcused bool) ([]item, *tally, error) {
	var items []item
	t := &tally{}

	for _, arg := range s.paths {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		path, err := filepath.Abs(arg)
		if err != nil {
			return nil, nil, err
		}
		lib, ok := s.libraryFor(path)
		if !ok {
			return nil, nil, fmt.Errorf("%s is in no configured library", arg)
		}
		if !scan.Includes(lib, path) {
			return nil, nil, fmt.Errorf("%s is not a file library %q covers", arg, lib.Name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, nil, err
		}
		t.total++

		it, err := s.consider(ctx, lib, s.cfg.Profile(lib), path, info, includeExcused, t)
		if err != nil {
			// Named a file and could not judge it: no verdict was reached, so
			// this is a fault rather than an answer about the media, and a
			// retry is worth something.
			return nil, nil, fmt.Errorf("%s: %w", arg, err)
		}
		if it != nil {
			items = append(items, *it)
		}
	}
	return items, t, nil
}

// Unlike a named file, a changed path failing to be judged is no fault: it may
// be gone again, or half a download whose next write names it again.
func (s *session) collectChanged(ctx context.Context, paths []string, includeExcused bool) ([]item, *tally, error) {
	var items []item
	t := &tally{}

	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		lib, path, ok := s.covering(path)
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		t.total++

		it, err := s.consider(ctx, lib, s.cfg.Profile(lib), path, info, includeExcused, t)
		if err != nil {
			t.unreadable++
			fmt.Fprintf(os.Stderr, "  ! %s: %v\n", rel(lib, path), err)
			continue
		}
		if it != nil {
			items = append(items, *it)
		}
	}
	if s.ownsCache {
		return items, t, s.store.Save()
	}
	return items, t, nil
}

// The path comes back in the form a full pass gives it, joined onto the
// library's own path, because the cache and excuses key on the string.
func (s *session) covering(path string) (config.Library, string, bool) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return config.Library{}, "", false
	}
	lib, ok := s.libraryFor(abs)
	if !ok || (s.library != "" && lib.Name != s.library) {
		return config.Library{}, "", false
	}
	root, err := filepath.Abs(lib.Path)
	if err != nil {
		return config.Library{}, "", false
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return config.Library{}, "", false
	}
	path = filepath.Join(lib.Path, rel)
	return lib, path, scan.Includes(lib, path)
}

// The most specific library wins, so one nested inside another's tree still
// gets its own profile.
func (s *session) libraryFor(path string) (config.Library, bool) {
	var best config.Library
	bestLen := -1
	for _, lib := range s.cfg.Libraries {
		root, err := filepath.Abs(lib.Path)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		if len(root) > bestLen {
			best, bestLen = lib, len(root)
		}
	}
	return best, bestLen >= 0
}

type tally struct {
	total, none, remux, transcode, excused, unreadable int
}

func (t *tally) count(a plan.Action) {
	switch a {
	case plan.ActionNone:
		t.none++
	case plan.ActionRemux:
		t.remux++
	case plan.ActionTranscode:
		t.transcode++
	}
}

func (t *tally) print() {
	fmt.Printf("\n%d files — %d conformant, %d remux, %d transcode",
		t.total, t.none, t.remux, t.transcode)
	if t.excused > 0 {
		fmt.Printf(", %d excused", t.excused)
	}
	if t.unreadable > 0 {
		fmt.Printf(", %d unreadable", t.unreadable)
	}
	fmt.Println()
}

func cmdProbe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	ffprobe := fs.String("ffprobe", "ffprobe", "ffprobe binary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("probe takes exactly one file")
	}
	f, err := media.NewProber(*ffprobe).Probe(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(f)
}

func planFlags(fs *flag.FlagSet) (cfg, lib *string, limit *int, verbose *bool) {
	cfg = fs.String("config", "conform.yaml", "config file")
	lib = fs.String("library", "", "restrict to one library")
	limit = fs.Int("limit", 0, "stop after N files needing work")
	verbose = fs.Bool("verbose", false, "log ffmpeg command lines")
	return
}

func cmdPlan(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	cfgPath, library, limit, verbose := planFlags(fs)
	showExcused := fs.Bool("show-excused", false, "plan excused files as -retry-excused would")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := newSession(*cfgPath, *library, fs.Args(), *limit, *verbose)
	if err != nil {
		return err
	}

	items, t, err := s.selection(ctx, *showExcused, nil)
	if err != nil {
		return err
	}
	for _, it := range items {
		describe(it)
		switch {
		case !*verbose:
		case it.plan.Unresolved():
			fmt.Println("          $ (no command yet: the worker that runs it chooses the encoder)")
		default:
			fmt.Printf("          $ %s %v\n", s.cfg.Execution.FFmpeg, it.plan.FFmpegArgs(it.plan.File.Path, "OUTPUT"+media.Ext(it.plan.Container)))
		}
	}
	t.print()
	return nil
}

func describe(it item) {
	fmt.Printf("%-9s %s\n", it.plan.Action, rel(it.lib, it.plan.File.Path))
	if it.excusedEncode != "" {
		fmt.Printf("          · encode excused: %s\n", it.excusedEncode)
	}
	for _, why := range it.plan.Reasons {
		fmt.Printf("          · %s\n", why)
	}
	for _, d := range it.plan.Dropped {
		fmt.Printf("          − drop %s stream %d (%s)\n", d.Type, d.Source, d.Reason)
	}
	if enc := encodes(it.plan, it.prof.Video.Encoder.Accel); enc != "" {
		fmt.Printf("          → %s\n", enc)
	}
}

// A software fallback on a node picked for its device is both far slower and a
// sign the placement is wrong, so the video encoder says which it is.
func encodes(p *plan.Plan, accel []string) string {
	var out []string
	for _, s := range p.Streams {
		if s.Codec == plan.Copy {
			continue
		}
		what := fmt.Sprintf("%s %s", s.Type, s.Codec)
		if s.Preset != "" && s.Codec == "" {
			if len(accel) == 0 {
				for _, a := range encoder.Order {
					accel = append(accel, string(a))
				}
			}
			what = fmt.Sprintf("%s %s (first that works of %s)", s.Type, s.Preset, strings.Join(accel, ", "))
		} else if s.Type == media.Video {
			hw := hwaccel(p.InputArgs)
			if hw == "" {
				// An encoder can drive a device without a hardware decode
				// flag to go with it, and reads as software without this.
				hw, _ = p.HWDevice()
			}
			if hw == "" {
				hw = "software"
			}
			what += " (" + hw + ")"
		}
		out = append(out, what)
	}
	if len(out) == 0 {
		return ""
	}
	return "encode " + strings.Join(out, ", ")
}

func hwaccel(inputArgs []string) string {
	for i, a := range inputArgs {
		if a == "-hwaccel" && i+1 < len(inputArgs) {
			return inputArgs[i+1]
		}
	}
	return ""
}

func cmdApply(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	cfgPath, library, limit, verbose := planFlags(fs)
	dryRun := fs.Bool("dry-run", false, "plan only")
	retryExcused := fs.Bool("retry-excused", false, "reconsider previously excused files")
	interval := fs.Duration("interval", 0, "repeat forever, waiting this long between passes")
	ffmpegOut := fs.Bool("ffmpeg-output", false, "stream ffmpeg's own output while it runs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	s, err := newSession(*cfgPath, *library, fs.Args(), *limit, *verbose)
	if err != nil {
		return err
	}
	s.ffmpegOut = *ffmpegOut

	tr, err := s.triggers(ctx, *dryRun)
	if err != nil {
		return err
	}
	return repeat(ctx, *interval, tr, func(changed []string) error {
		return s.pass(ctx, *dryRun, *retryExcused, changed)
	})
}

// Long enough that a download writing in bursts is not caught between them.
// Not a setting: the check before commit catches the case this misses.
const settle = time.Minute

type triggers struct {
	watch *watch.Watcher
	hook  *webhook.Server
}

// A one-off run exits after its pass. That includes a worker Job handed one
// path, which reads the same config and would otherwise never finish.
func (s *session) oneOff(dryRun bool) bool {
	return dryRun || len(s.paths) > 0 || s.limit != 0
}

// Started before the first pass, so a file arriving during it is not missed.
func (s *session) triggers(ctx context.Context, dryRun bool) (*triggers, error) {
	tr := &triggers{}
	if s.oneOff(dryRun) {
		return tr, nil
	}
	covered := func(path string) bool {
		_, _, ok := s.covering(path)
		return ok
	}

	var roots []string
	for _, lib := range s.cfg.Libraries {
		if lib.Watch && (s.library == "" || lib.Name == s.library) {
			roots = append(roots, lib.Path)
		}
	}
	if len(roots) > 0 {
		w, err := watch.Start(ctx, roots, settle, covered)
		if err != nil {
			return nil, err
		}
		tr.watch = w
		fmt.Printf("watching %s for new files\n", strings.Join(roots, ", "))
	}

	if s.cfg.Webhook.Listen != "" {
		h, err := webhook.Start(ctx, s.cfg.Webhook, covered)
		if err != nil {
			tr.close()
			return nil, err
		}
		tr.hook = h
		fmt.Printf("listening for webhooks on %s: %s\n", h.Addr(), strings.Join(webhook.Routes(), ", "))
	}
	return tr, nil
}

func (tr *triggers) close() {
	if tr.watch != nil {
		tr.watch.Close()
	}
	if tr.hook != nil {
		tr.hook.Close()
	}
}

// One pass runs at a time, so no file is ever in two at once. A change that
// arrives during a long pass waits for the next batch rather than being lost.
func repeat(ctx context.Context, interval time.Duration, tr *triggers, pass func(changed []string) error) error {
	defer tr.close()
	var watched, hooked <-chan []string
	var lost <-chan struct{}
	var watchErr, hookErr <-chan error
	if tr.watch != nil {
		watched, lost, watchErr = tr.watch.Changed(), tr.watch.Lost(), tr.watch.Err()
	}
	if tr.hook != nil {
		hooked, hookErr = tr.hook.Changed(), tr.hook.Err()
	}

	for {
		if err := pass(nil); err != nil {
			return err
		}
		if interval == 0 && tr.watch == nil && tr.hook == nil {
			return nil
		}
		var next <-chan time.Time
		if interval > 0 {
			fmt.Printf("\nnext pass in %s\n", interval)
			next = time.After(interval)
		}

	wait:
		for {
			var paths []string
			select {
			case <-ctx.Done():
				return ctx.Err()
			case err := <-watchErr:
				return fmt.Errorf("watch: %w", err)
			case err := <-hookErr:
				return fmt.Errorf("webhook: %w", err)
			case paths = <-watched:
			case paths = <-hooked:
			case <-lost:
				fmt.Println("\nfile events were lost; walking every library")
				break wait
			case <-next:
				break wait
			}
			if err := pass(paths); err != nil {
				return err
			}
		}
	}
}

func cmdOrchestrate(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("orchestrate", flag.ExitOnError)
	cfgPath, library, limit, verbose := planFlags(fs)
	dryRun := fs.Bool("dry-run", false, "build the jobs and create none")
	kubeconfig := fs.String("kubeconfig", "", "kubeconfig to use instead of the in-cluster account")
	interval := fs.Duration("interval", 0, "repeat forever, waiting this long between passes")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return errors.New("orchestrate plans libraries and names the files itself; it takes no file arguments")
	}
	s, err := newSession(*cfgPath, *library, nil, *limit, *verbose)
	if err != nil {
		return err
	}

	// Needed even to dry-run: the Job is the profile's PodTemplate with the
	// path appended, so there is nothing to render without reading it.
	kube, err := orchestrate.NewKube(*kubeconfig)
	if err != nil {
		return err
	}
	o := &orchestrate.Orchestrator{Kube: kube, Opts: s.cfg.Orchestrator, DryRun: *dryRun}
	tr, err := s.triggers(ctx, *dryRun)
	if err != nil {
		return err
	}
	return repeat(ctx, *interval, tr, func(changed []string) error { return s.dispatch(ctx, o, changed) })
}

func (s *session) dispatch(ctx context.Context, o *orchestrate.Orchestrator, changed []string) error {
	items, t, err := s.selection(ctx, false, changed)
	if err != nil || quiet(changed, items, t) {
		return err
	}

	reqs := make([]orchestrate.Request, 0, len(items))
	for _, it := range items {
		reqs = append(reqs, orchestrate.Request{
			File: it.plan.File, Profile: it.prof, Library: it.lib.Name,
			VideoEncode: it.plan.EncodesVideo(),
		})
	}

	// Reported before the error is returned: a dispatch that stops partway has
	// already created Jobs, and they are not undone by failing here.
	out, dispatchErr := o.Dispatch(ctx, reqs)
	for i, d := range out {
		name := d.Path
		if i < len(items) {
			name = rel(items[i].lib, d.Path)
		}
		if d.Job == nil {
			fmt.Printf("%-9s %s\n", d.Outcome, name)
			continue
		}
		fmt.Printf("%-9s %s → %s\n", d.Outcome, name, d.Name())
		if s.verbose {
			y, err := yaml.Marshal(d.Job)
			if err != nil {
				return err
			}
			fmt.Printf("---\n%s", y)
		}
	}
	if dispatchErr != nil {
		return dispatchErr
	}
	t.print()
	return nil
}

// Every replaced file raises an event and comes back conformant, so a batch
// with nothing to report says nothing rather than confirming that each time.
func quiet(changed []string, items []item, t *tally) bool {
	return changed != nil && len(items) == 0 && t.unreadable == 0
}

func (s *session) pass(ctx context.Context, dryRun, retryExcused bool, changed []string) error {
	items, t, err := s.selection(ctx, retryExcused, changed)
	if err != nil || quiet(changed, items, t) {
		return err
	}
	if dryRun {
		for _, it := range items {
			fmt.Printf("%-9s %s — %s\n", it.plan.Action, rel(it.lib, it.plan.File.Path), it.plan)
		}
		t.print()
		return nil
	}

	// A worker that hits a fatal error stops reading, so the send below would
	// block forever on a live context.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	runner := &run.Runner{Exec: s.cfg.Execution, Prober: s.prober, Store: s.store}
	if s.verbose {
		runner.Logf = func(f string, a ...any) { fmt.Printf("          $ "+f+"\n", a...) }
	}
	if s.ffmpegOut {
		runner.FFmpegOutput = os.Stderr
	}

	work := make(chan item)
	var wg sync.WaitGroup
	var mu sync.Mutex
	runner.Notef = func(f string, a ...any) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Printf(f+"\n", a...)
	}
	var failures int
	var fatal error

	for range s.cfg.Execution.Workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range work {
				mu.Lock()
				describe(it)
				mu.Unlock()
				res, err := runner.Apply(ctx, it.plan, it.prof, it.excusedEncode)
				mu.Lock()
				if err != nil {
					// Not once the context is done: the error is then this
					// run being cancelled, reported once by pass itself.
					if fatal == nil && ctx.Err() == nil {
						fatal = fmt.Errorf("%s: %w", rel(it.lib, it.plan.File.Path), err)
					}
					mu.Unlock()
					cancel()
					return
				}
				report(it, res)
				if res.Outcome == run.OutcomeFailed {
					failures++
				}
				mu.Unlock()
			}
		}()
	}

	for _, it := range items {
		select {
		case <-ctx.Done():
			close(work)
			wg.Wait()
			if fatal != nil {
				return fatal
			}
			return ctx.Err()
		case work <- it:
		}
	}
	close(work)
	wg.Wait()

	if fatal != nil {
		return fatal
	}
	if s.ownsCache {
		if err := s.store.Save(); err != nil {
			return err
		}
	}
	t.print()
	// A file that cannot be processed is a verdict, already recorded in the
	// excuse ledger. Failing the run for it would have the ledger and a job
	// runner's own retries multiply.
	if failures > 0 {
		fmt.Fprintf(os.Stderr, "%d file(s) could not be processed\n", failures)
	}
	return nil
}

func report(it item, res run.Result) {
	name := rel(it.lib, res.Path)
	if res.Excused != "" {
		fmt.Printf("%-9s %s — %s\n", "excused", rel(it.lib, it.plan.File.Path), res.Excused)
	}
	switch res.Outcome {
	case run.OutcomeReplaced:
		saved := ""
		if res.Before > 0 && res.After > 0 {
			saved = fmt.Sprintf(" %s → %s (%+.0f%%)", human(res.Before), human(res.After),
				(float64(res.After)/float64(res.Before)-1)*100)
		}
		with := ""
		if res.Encoder != "" {
			with = " with " + res.Encoder
		}
		fmt.Printf("%-9s %s%s in %s%s\n", res.Action, name, saved, res.Duration.Round(time.Second), with)
	case run.OutcomeExcused:
		fmt.Printf("%-9s %s — %s\n", "excused", name, res.Detail)
	case run.OutcomeFailed:
		fmt.Printf("%-9s %s\n%s\n", "FAILED", name, indent(res.Detail))
	}
}

func rel(lib config.Library, path string) string {
	if r, err := filepath.Rel(lib.Path, path); err == nil {
		return r
	}
	return path
}

func human(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%cB", float64(b)/float64(div), "KMGTP"[exp])
}

func indent(s string) string {
	out := ""
	for _, line := range splitLines(s) {
		out += "          " + line + "\n"
	}
	return out
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
