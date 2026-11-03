package hooks

import "testing"

// The decision itself, separated from ffprobe so it can be pinned here: an
// animation is anything with more than one frame. A one-frame video container
// is a still, a many-frame WebP is an animation — the extension has no say.
func TestFiletypeForFrameCount(t *testing.T) {
	cases := map[int]string{0: "image", 1: "image", 2: "gif", 300: "gif"}
	for frames, want := range cases {
		if got := filetypeForFrameCount(frames); got != want {
			t.Errorf("filetypeForFrameCount(%d) = %q, want %q", frames, got, want)
		}
	}
}

func TestParseFrameCount(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want int
	}{
		{"1\n", 1},
		{"421\n\n421\n", 421}, // FFmpeg 9, repeated stream entry
		{"37,\n", 37},
		{"\nN/A\n12\n", 12},
	} {
		got, err := parseFrameCount(tc.out)
		if err != nil || got != tc.want {
			t.Errorf("parseFrameCount(%q) = %d, %v; want %d", tc.out, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "\n\n", "N/A\n"} {
		if _, err := parseFrameCount(bad); err == nil {
			t.Errorf("parseFrameCount(%q): want an error", bad)
		}
	}
}
