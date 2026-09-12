package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/edjeffreys/conform/internal/media"
	"github.com/edjeffreys/conform/internal/plan"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReplaceRefusesToOverwriteUnrelatedFile(t *testing.T) {
	dir := t.TempDir()
	bystander := filepath.Join(dir, "Film.mkv")
	src := filepath.Join(dir, "Film.mp4")
	tmp := filepath.Join(t.TempDir(), "encoded.mkv")

	write(t, bystander, "the original mkv")
	write(t, src, "the mp4")
	write(t, tmp, "the transcode")

	r := &Runner{}
	if _, err := r.replace(tmp, src, "mkv"); err == nil {
		t.Fatal("replace succeeded over an unrelated file; want an error")
	}

	if got := read(t, bystander); got != "the original mkv" {
		t.Errorf("bystander was modified: %q", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("source was removed despite the failure: %v", err)
	}
}

func TestReplaceOverwritesItsOwnSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Film.mkv")
	tmp := filepath.Join(t.TempDir(), "encoded.mkv")

	write(t, src, "before")
	write(t, tmp, "after")

	r := &Runner{}
	final, err := r.replace(tmp, src, "mkv")
	if err != nil {
		t.Fatal(err)
	}
	if final != src {
		t.Errorf("final = %q, want %q", final, src)
	}
	if got := read(t, src); got != "after" {
		t.Errorf("content = %q, want %q", got, "after")
	}
}

func TestReplaceContainerChangeRemovesSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Film.avi")
	tmp := filepath.Join(t.TempDir(), "encoded.mkv")

	write(t, src, "before")
	write(t, tmp, "after")

	r := &Runner{}
	final, err := r.replace(tmp, src, "mkv")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "Film.mkv"); final != want {
		t.Fatalf("final = %q, want %q", final, want)
	}
	if got := read(t, final); got != "after" {
		t.Errorf("content = %q, want %q", got, "after")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Errorf("superseded source still present: %v", err)
	}
}

// No staging file may survive a run, successful or not — the scanner skips
// dotfiles, so a leftover would be invisible rather than merely untidy.
func TestReplaceLeavesNoStagingFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Film.avi")
	tmp := filepath.Join(t.TempDir(), "encoded.mkv")
	write(t, src, "before")
	write(t, tmp, "after")

	r := &Runner{}
	if _, err := r.replace(tmp, src, "mkv"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".mkv" || e.Name()[0] == '.' {
			t.Errorf("unexpected leftover %q", e.Name())
		}
	}
}

// The copy this avoids is a second full pass over a file of tens of GB.
func TestStageConsumesTheTempFileWhenItCanRename(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "encoded.mkv")
	staging := filepath.Join(dir, ".conform-abc.mkv")
	write(t, tmp, "the transcode")

	if err := stage(tmp, staging); err != nil {
		t.Fatal(err)
	}
	if got := read(t, staging); got != "the transcode" {
		t.Errorf("staged %q, want the transcode", got)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("the temp file survived, so this was a copy and not a rename")
	}
}

// The opposite of the above, and the caller's deferred remove relies on it.
func TestStageFallsBackToACopyThatKeepsTheSource(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "encoded.mkv")
	staging := filepath.Join(dir, ".conform-abc.mkv")
	write(t, tmp, "the transcode")

	if err := copyFile(tmp, staging); err != nil {
		t.Fatal(err)
	}
	if got := read(t, staging); got != "the transcode" {
		t.Errorf("staged %q, want the transcode", got)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("the temp file should survive a copy: %v", err)
	}
}

func TestStageReportsAFailureThatIsNotCrossDevice(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "encoded.mkv")
	write(t, tmp, "the transcode")

	// A missing parent fails for a reason no copy would recover from.
	if err := stage(tmp, filepath.Join(dir, "absent", "x.mkv")); err == nil {
		t.Error("stage succeeded into a missing directory")
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("a failed stage consumed the temp file: %v", err)
	}
}

func TestDestinationFree(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Taken.mkv"), "x")

	cases := []struct {
		name      string
		src       string
		container string
		want      bool
	}{
		{"same container is always its own destination", "Taken.mkv", "mkv", true},
		{"container change onto an occupied name", "Taken.mp4", "mkv", false},
		{"container change onto a free name", "Other.mp4", "mkv", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, got := destinationFree(filepath.Join(dir, c.src), c.container); got != c.want {
				t.Errorf("destinationFree(%q, %q) = %v, want %v", c.src, c.container, got, c.want)
			}
		})
	}
}

func TestPathHashDistinguishesExtensions(t *testing.T) {
	if pathHash("/media/Film.mkv") == pathHash("/media/Film.mp4") {
		t.Error("paths differing only by extension hash alike")
	}
}

func TestUnchangedCatchesASourceWrittenDuringTheEncode(t *testing.T) {
	src := filepath.Join(t.TempDir(), "Film.mkv")
	write(t, src, "first chunk")
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	f := &media.File{Path: src, Size: info.Size(), ModTime: info.ModTime()}

	if detail, ok := unchanged(f); !ok {
		t.Fatalf("an untouched source reads as changed: %s", detail)
	}

	write(t, src, "first chunk, second chunk")
	if _, ok := unchanged(f); ok {
		t.Error("a source that grew reads as unchanged")
	}

	os.Remove(src)
	if _, ok := unchanged(f); ok {
		t.Error("a source that is gone reads as unchanged")
	}
}

func TestLostDepth(t *testing.T) {
	video := func(i int, codec, pix string) media.Stream {
		return media.Stream{Index: i, Type: media.Video, Codec: codec, PixFmt: pix}
	}
	poster := media.Stream{Type: media.Video, Codec: "mjpeg", AttachedPic: true}
	encode := func(source int) plan.StreamPlan {
		return plan.StreamPlan{Source: source, Type: media.Video, Codec: "hevc_vaapi"}
	}
	copied := plan.StreamPlan{Type: media.Video, Codec: plan.Copy}

	tests := []struct {
		name   string
		source []media.Stream
		plans  []plan.StreamPlan
		output []media.Stream
		lost   bool
	}{
		{
			name:   "10-bit encoded down to 8",
			source: []media.Stream{video(0, "h264", "yuv420p10le")},
			plans:  []plan.StreamPlan{encode(0)},
			output: []media.Stream{video(0, "hevc", "yuv420p")},
			lost:   true,
		},
		{
			name:   "10-bit kept",
			source: []media.Stream{video(0, "h264", "yuv420p10le")},
			plans:  []plan.StreamPlan{encode(0)},
			output: []media.Stream{video(0, "hevc", "p010le")},
		},
		{
			name:   "8-bit raised to 10",
			source: []media.Stream{video(0, "h264", "yuv420p")},
			plans:  []plan.StreamPlan{encode(0)},
			output: []media.Stream{video(0, "hevc", "yuv420p10le")},
		},
		{
			name:   "unknown source depth",
			source: []media.Stream{video(0, "h264", "")},
			plans:  []plan.StreamPlan{encode(0)},
			output: []media.Stream{video(0, "hevc", "yuv420p")},
		},
		{
			name:   "copied stream",
			source: []media.Stream{video(0, "hevc", "yuv420p10le")},
			plans:  []plan.StreamPlan{copied},
			output: []media.Stream{video(0, "hevc", "yuv420p")},
		},
		{
			// Matroska stores the poster as an attachment, which the probe drops,
			// so pairing streams by position alone would miss the loss.
			name:   "cover art before the video",
			source: []media.Stream{poster, video(1, "h264", "yuv420p10le")},
			plans:  []plan.StreamPlan{copied, encode(1)},
			output: []media.Stream{video(0, "hevc", "yuv420p")},
			lost:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := &plan.Plan{File: &media.File{Streams: tc.source}, Streams: tc.plans}
			got := lostDepth(p, &media.File{Streams: tc.output})
			if (got != "") != tc.lost {
				t.Errorf("lostDepth = %q, want lost = %v", got, tc.lost)
			}
		})
	}
}
