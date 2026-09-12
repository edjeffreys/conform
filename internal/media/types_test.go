package media

import "testing"

func TestBitDepth(t *testing.T) {
	for pix, want := range map[string]int{
		"":        0,
		"yuv420p": 8, "yuvj420p": 8, "nv12": 8, "nv16": 8, "rgb24": 8, "pal8": 8,
		"yuv420p10le": 10, "p010le": 10, "x2rgb10le": 10,
		"yuv422p12le": 12, "gray12be": 12,
		"yuv444p16le": 16, "p016le": 16,
	} {
		if got := (Stream{PixFmt: pix}).BitDepth(); got != want {
			t.Errorf("%q: got %d, want %d", pix, got, want)
		}
	}
}
