package main

import "testing"

func TestMeasureStickerNeedsNoProbe(t *testing.T) {
	// A sticker's dimensions are known from the pipeline's crop+scale filter, so
	// measure must not reach for the network at all — and must not report the
	// source's ratio, which the square crop discarded.
	w, h, err := measure(record{Id: "x", Filetype: "sticker", Original: ""})
	if err != nil {
		t.Fatalf("errored on a sticker with no URL: %v", err)
	}
	if w != stickerDimensions || h != stickerDimensions {
		t.Errorf("got %dx%d, want %dx%d", w, h, stickerDimensions, stickerDimensions)
	}
}

func TestMeasureRejectsARecordWithNothingToProbe(t *testing.T) {
	// A record whose encode never produced a rendition has no URL to measure.
	// Failing it (and re-running later) is right; storing zeroes would make the
	// re-run skip it forever.
	if _, _, err := measure(record{Id: "x", Filetype: "gif"}); err == nil {
		t.Fatal("want an error when there is no original or preview")
	}
}

func TestParseDimensions(t *testing.T) {
	// Duplicated from hooks.parseDimensions on purpose (the script probes a URL,
	// not bytes), so it carries its own coverage of the same ffprobe quirks.
	tests := []struct {
		name    string
		in      string
		w, h    int
		wantErr bool
	}{
		{name: "landscape", in: "1920,1080\n", w: 1920, h: 1080},
		{name: "portrait", in: "608,1080", w: 608, h: 1080},
		{name: "trailing comma", in: "1280,720,\n", w: 1280, h: 720},
		{name: "skips unusable line", in: "N/A,N/A\n800,600\n", w: 800, h: 600},
		{name: "zeroes are not a measurement", in: "0,0\n", wantErr: true},
		{name: "empty", in: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h, err := parseDimensions(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDimensions(%q) = %d,%d, want error", tt.in, w, h)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDimensions(%q) errored: %v", tt.in, err)
			}
			if w != tt.w || h != tt.h {
				t.Errorf("parseDimensions(%q) = %d,%d, want %d,%d", tt.in, w, h, tt.w, tt.h)
			}
		})
	}
}
