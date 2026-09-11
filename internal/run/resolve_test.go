package run

import (
	"errors"
	"slices"
	"testing"

	"github.com/edjeffreys/conform/internal/encoder"
)

func TestPickTakesTheFirstThatWorks(t *testing.T) {
	cands := encoder.Candidates("hevc", []encoder.Accel{encoder.QSV, encoder.VAAPI, encoder.Software})
	devices := func(a encoder.Accel) ([]string, string) {
		switch a {
		case encoder.QSV:
			return nil, "no render node"
		case encoder.VAAPI:
			return []string{"/dev/dri/renderD128", "/dev/dri/renderD129"}, ""
		}
		return []string{""}, ""
	}
	var ran []string
	try := func(c choice) (string, error) {
		ran = append(ran, c.Name+c.Device)
		if c.Device == "/dev/dri/renderD129" {
			return "the second GPU", nil
		}
		return "", errors.New("no")
	}

	c, tried, ok := pick(cands, devices, try)
	if !ok || c.Name != "hevc_vaapi" || c.Device != "/dev/dri/renderD129" || c.About != "the second GPU" {
		t.Fatalf("picked %+v, %v", c, ok)
	}
	if len(tried) != 2 || tried[0].why != "no render node" || tried[1].Device != "/dev/dri/renderD128" {
		t.Errorf("tried = %+v", tried)
	}
	if want := []string{"hevc_vaapi/dev/dri/renderD128", "hevc_vaapi/dev/dri/renderD129"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v: nothing after the first success", ran, want)
	}
}

func TestPickReportsEveryFailure(t *testing.T) {
	cands := encoder.Candidates("av1", []encoder.Accel{encoder.NVENC, encoder.Software})
	_, tried, ok := pick(cands, func(encoder.Accel) ([]string, string) { return []string{"0"}, "" },
		func(choice) (string, error) { return "", errors.New("no") })
	if ok || len(tried) != 2 {
		t.Errorf("ok = %v, tried = %+v", ok, tried)
	}
}

func TestAbout(t *testing.T) {
	tests := map[string]string{
		"[AVHWDeviceContext @ 0x5] VAAPI driver: Intel iHD driver for Intel(R) Gen Graphics - 25.1.4 ().": "Intel iHD driver for Intel(R) Gen Graphics - 25.1.4",
		"[hevc_nvenc @ 0x5] [ GPU #0 - < NVIDIA GeForce RTX 3060 > has Compute SM 8.6 ]":                  "NVIDIA GeForce RTX 3060",
		"frame=    5 fps=0.0 q=-0.0 Lsize=N/A":                                                            "",
	}
	for line, want := range tests {
		if got := about("noise\n" + line + "\nmore noise"); got != want {
			t.Errorf("about(%q) = %q, want %q", line, got, want)
		}
	}
}
