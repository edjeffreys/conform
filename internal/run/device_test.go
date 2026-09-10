package run

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/edjeffreys/conform/internal/config"
)

// The distinction the excuse ledger depends on: a file ffmpeg could not encode
// is the file's problem, a device it could not open is the worker's.
func TestDeviceFault(t *testing.T) {
	worker := []string{
		"[AVHWDeviceContext] Error creating a MFX session: -9.",
		"Device creation failed: -1313558101.",
		"[dec:h264_qsv] No device available for decoder: device type qsv needed for codec h264_qsv",
		"Failed to set value 'qsv=hw' for option 'init_hw_device': Function not implemented",
		"[AVHWDeviceContext] No VA display found for device /dev/dri/renderD128.",
	}
	for _, s := range worker {
		if deviceFault(s) == "" {
			t.Errorf("read as a media failure, want a worker fault: %q", s)
		}
	}

	badFile := []string{
		"[matroska,webm] Read error at pos. 4096 (0x1000)",
		"Invalid data found when processing input",
		"[hevc_qsv] Error during encoding: incompatible video parameters",
		"Conversion failed!",
	}
	for _, s := range badFile {
		if sig := deviceFault(s); sig != "" {
			t.Errorf("read as a worker fault (%q), want a media failure: %q", sig, s)
		}
	}
}

func stubFFmpeg(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckDeviceFailsTheWorkerNotTheFile(t *testing.T) {
	bin := stubFFmpeg(t, `echo "Error creating a MFX session: -9." >&2; exit 1`)
	r := &Runner{Exec: config.Execution{FFmpeg: bin}}

	err := r.checkDevice(context.Background(), "qsv=conform")
	if err == nil {
		t.Fatal("checkDevice accepted a device that could not be created")
	}
}

// A hundred files must not each pay for the same answer, and a device that
// fails once has not changed by the next file.
func TestCheckDeviceAsksOnce(t *testing.T) {
	log := filepath.Join(t.TempDir(), "calls")
	bin := stubFFmpeg(t, `echo x >> `+log+`; exit 0`)
	r := &Runner{Exec: config.Execution{FFmpeg: bin}}

	for range 3 {
		if err := r.checkDevice(context.Background(), "vaapi=conform"); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if calls := strings.Count(string(b), "x"); calls != 1 {
		t.Errorf("ffmpeg ran %d times, want 1", calls)
	}
}
