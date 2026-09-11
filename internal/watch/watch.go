// Package watch reports files that appear or change under directory trees, once
// they stop changing. A path is a hint, not a verdict: events can be lost or
// never sent at all, so a missed one costs latency and a full pass recovers it.
package watch

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/edjeffreys/conform/internal/scan"
)

type Watcher struct {
	fsw   *fsnotify.Watcher
	match func(path string) bool

	changed chan []string
	lost    chan struct{}
	failed  chan error
}

func Start(ctx context.Context, roots []string, settle time.Duration, match func(path string) bool) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watcher{
		fsw: fsw, match: match,
		changed: make(chan []string),
		lost:    make(chan struct{}, 1),
		failed:  make(chan error, 1),
	}
	p := newPending(settle)
	for _, root := range roots {
		if err := w.addTree(root, true, nil); err != nil {
			fsw.Close()
			return nil, err
		}
	}
	go w.loop(ctx, p)
	return w, nil
}

// Paths settling while the receiver is busy accumulate into its next batch.
func (w *Watcher) Changed() <-chan []string { return w.changed }

// Events were dropped, and only a full pass recovers what they described.
func (w *Watcher) Lost() <-chan struct{} { return w.lost }

func (w *Watcher) Err() <-chan error { return w.failed }

func (w *Watcher) Close() error { return w.fsw.Close() }

func (w *Watcher) loop(ctx context.Context, p *pending) {
	timer := time.NewTimer(0)
	<-timer.C
	ready := map[string]bool{}

	rearm := func() {
		timer.Stop()
		if at, ok := p.next(); ok {
			timer.Reset(time.Until(at))
		}
	}

	for {
		var out chan []string
		var batch []string
		if len(ready) > 0 {
			out = w.changed
			batch = sortedKeys(ready)
		}

		select {
		case <-ctx.Done():
			return

		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			if err := w.handle(ev, p, ready); err != nil {
				w.failed <- err
				return
			}
			rearm()

		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			if errors.Is(err, fsnotify.ErrEventOverflow) {
				select {
				case w.lost <- struct{}{}:
				default:
				}
				continue
			}
			w.failed <- err
			return

		case <-timer.C:
			for _, path := range p.settled(time.Now()) {
				ready[path] = true
			}
			rearm()

		case out <- batch:
			clear(ready)
		}
	}
}

func (w *Watcher) handle(ev fsnotify.Event, p *pending, ready map[string]bool) error {
	switch {
	case ev.Has(fsnotify.Create):
		info, err := os.Lstat(ev.Name)
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if scan.SkipDir(filepath.Base(ev.Name)) {
				return nil
			}
			// Moving a directory in raises one event for the directory and
			// none for what it holds, so its contents are found by walking.
			return w.addTree(ev.Name, false, func(path string) { w.touch(path, p, ready) })
		}
		w.touch(ev.Name, p, ready)

	case ev.Has(fsnotify.Write):
		w.touch(ev.Name, p, ready)

	case ev.Has(fsnotify.Remove), ev.Has(fsnotify.Rename):
		p.forget(ev.Name)
		for path := range ready {
			if within(ev.Name, path) {
				delete(ready, path)
			}
		}
		// Already gone on inotify; kqueue keeps a renamed directory's watch
		// under its old name.
		w.fsw.Remove(ev.Name)
	}
	return nil
}

func (w *Watcher) touch(path string, p *pending, ready map[string]bool) {
	if !w.match(path) {
		return
	}
	delete(ready, path)
	p.touch(path, time.Now())
}

// The directory is watched before it is walked, so a file landing in between
// raises an event rather than falling through the gap.
func (w *Watcher) addTree(root string, isRoot bool, found func(path string)) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			if found != nil {
				found(path)
			}
			return nil
		}
		if !(isRoot && path == root) && scan.SkipDir(d.Name()) {
			return fs.SkipDir
		}
		if err := w.fsw.Add(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipDir
			}
			if errors.Is(err, syscall.ENOSPC) {
				return fmt.Errorf("watch %s: out of inotify watches, one per directory; raise fs.inotify.max_user_watches", path)
			}
			return fmt.Errorf("watch %s: %w", path, err)
		}
		return nil
	})
}

func within(dir, path string) bool {
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// A download raises a write per chunk, so a path settles only once quiet.
type pending struct {
	settle time.Duration
	due    map[string]time.Time
}

func newPending(settle time.Duration) *pending {
	return &pending{settle: settle, due: map[string]time.Time{}}
}

func (p *pending) touch(path string, now time.Time) {
	p.due[path] = now.Add(p.settle)
}

// Removing a directory raises no event for the files inside it.
func (p *pending) forget(path string) {
	for k := range p.due {
		if within(path, k) {
			delete(p.due, k)
		}
	}
}

func (p *pending) settled(now time.Time) []string {
	var out []string
	for path, at := range p.due {
		if !at.After(now) {
			out = append(out, path)
			delete(p.due, path)
		}
	}
	slices.Sort(out)
	return out
}

func (p *pending) next() (time.Time, bool) {
	var earliest time.Time
	for _, at := range p.due {
		if earliest.IsZero() || at.Before(earliest) {
			earliest = at
		}
	}
	return earliest, !earliest.IsZero()
}
