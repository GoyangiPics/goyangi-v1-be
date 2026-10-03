package hooks

import "testing"

func TestParseDimensions(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		w, h    int
		wantErr bool
	}{
		{name: "landscape", in: "1920,1080\n", w: 1920, h: 1080},
		{name: "portrait fancam", in: "432,768\n", w: 432, h: 768},
		{name: "no trailing newline", in: "200,200", w: 200, h: 200},
		// Some ffmpeg builds append a separator for the requested-but-absent
		// entries, so the field count is not reliably two.
		{name: "trailing comma", in: "1280,720,\n", w: 1280, h: 720},
		{name: "surrounding whitespace", in: "  1280 , 720  \n", w: 1280, h: 720},
		// -select_streams v:0 still emits one line per stream on some builds when
		// a file carries several video streams (cover art, thumbnails). First
		// usable line wins.
		{name: "multiple stream lines", in: "1920,1080\n640,360\n", w: 1920, h: 1080},
		{name: "skips an unusable first line", in: "N/A,N/A\n1920,1080\n", w: 1920, h: 1080},

		// A probe that found a stream but not its size must not be stored as if
		// it were a measurement — the frontend reads 0 as "unknown" and falls
		// back to measuring the media, which is strictly better than laying out
		// against a zero-height box.
		{name: "zero dimensions", in: "0,0\n", wantErr: true},
		{name: "zero height only", in: "1920,0\n", wantErr: true},
		{name: "negative", in: "-1,-1\n", wantErr: true},
		{name: "not a number", in: "N/A,N/A\n", wantErr: true},
		{name: "one field", in: "1920\n", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "whitespace only", in: "  \n \n", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, h, err := parseDimensions(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseDimensions(%q) = %d,%d, want error", tt.in, w, h)
				}
				// A caller that ignores the error must not get a plausible-looking
				// size out of a failed parse.
				if w != 0 || h != 0 {
					t.Errorf("parseDimensions(%q) = %d,%d on error, want 0,0", tt.in, w, h)
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
