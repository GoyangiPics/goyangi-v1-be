package hooks

import (
	"strings"
	"testing"
)

// The AV1 box must stay square, for the reason TestStillScaleCap guards the
// still one: an asymmetric box reads min() of the SOURCE on both axes, so a
// portrait 1080×1920 gets boxed at 1080×1080 and encoded at ~608×1080 — a
// landscape cap silently applied to portrait content. The stills fixed this
// first; the AV1 original follows them.
func TestAV1ScaleCapIsOrientationAgnostic(t *testing.T) {
	for _, want := range []string{
		"min(iw,1920)",
		"min(ih,1920)",
		"force_original_aspect_ratio=decrease",
		// decrease is free to land on an odd edge, and this feeds a
		// yuv420p10le encode that errors on odd dimensions.
		"force_divisible_by=2",
	} {
		if !strings.Contains(scaleDown1080p, want) {
			t.Errorf("scaleDown1080p = %q, missing %q", scaleDown1080p, want)
		}
	}
	if strings.Contains(scaleDown1080p, "1080)") {
		t.Errorf("scaleDown1080p = %q, still carries the landscape-only 1080 cap", scaleDown1080p)
	}
}

// The SD box, by contrast, must stay ASYMMETRIC: it is the budget rendition,
// and portrait binding on the 720 height cap is its size budget at work — see
// scaleDown720p's comment. This pins the decision so the AV1 fix doesn't get
// "completed" over here by mistake.
func TestSDScaleCapStaysBudgetBoxed(t *testing.T) {
	for _, want := range []string{
		"min(iw,1280)",
		"min(ih,720)",
		"force_original_aspect_ratio=decrease",
		"force_divisible_by=2",
	} {
		if !strings.Contains(scaleDown720p, want) {
			t.Errorf("scaleDown720p = %q, missing %q", scaleDown720p, want)
		}
	}
}
