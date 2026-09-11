package media

import "testing"

func TestHighBitDepth(t *testing.T) {
	for pix, want := range map[string]bool{
		"yuv420p": false, "nv12": false, "yuvj420p": false, "rgb24": false, "": false,
		"yuv420p10le": true, "p010le": true, "yuv422p12le": true, "yuv444p16le": true,
	} {
		if got := (Stream{PixFmt: pix}).HighBitDepth(); got != want {
			t.Errorf("%q: got %v, want %v", pix, got, want)
		}
	}
}
