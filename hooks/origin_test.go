package hooks

import "testing"

func TestIsImgurMirror(t *testing.T) {
	yes := []string{
		"https://i.imgur.com/AbCd123.mp4",
		"https://imgur.com/AbCd123",
		" http://imgur.com/a/xyz ",
	}
	for _, m := range yes {
		if !isImgurMirror(m) {
			t.Errorf("isImgurMirror(%q) = false, want true", m)
		}
	}
	no := []string{
		"",
		"my cam",
		"https://files.catbox.moe/abc.mp4",
		"https://notimgur.com/x",
		"https://imgur.com.evil.example/x",
	}
	for _, m := range no {
		if isImgurMirror(m) {
			t.Errorf("isImgurMirror(%q) = true, want false", m)
		}
	}
}
