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
