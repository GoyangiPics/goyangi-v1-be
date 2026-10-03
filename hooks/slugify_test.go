package hooks

import "testing"

// Shared with the frontend: test/utils.test.ts carries this exact table.
// The FE's toSlug is a character-for-character port of Slugify — it predicts
// label slugs for optimistic UI (duplicate detection) that this function then
// computes for real, so a divergence makes the FE lie about whether a label
// exists. Change a vector only in both files at once.
var slugVectors = []struct {
	input string
	slug  string
}{
	{"IVE", "ive"},
	{"Jang Wonyoung", "jang-wonyoung"},
	{"Kwon Eunbi [IZ*ONE]", "kwon-eunbi-izone"},
	{"Cute!", "cute"},
	{"4:3", "43"},
	// Underscores are stripped, not kept — label "mirror_selca" stores slug
	// "mirrorselca", and the FE prediction has to agree.
	{"mirror_selca", "mirrorselca"},
	// Only literal spaces map to hyphens, one hyphen each — runs don't collapse.
	{"a   b", "a---b"},
	{"  spaced  out  ", "spaced--out"},
	// Other whitespace is stripped by the charset filter, not hyphenated.
	{"a\tb", "ab"},
	{"already-slugged", "already-slugged"},
	{"--trim--", "trim"},
	// Empty output is part of the contract: callers feeding a required field
	// (tags.code, labels.slug) must handle it — see the Slugify doc comment.
	{"!!!", ""},
	{"", ""},
	{"안유진", ""},
}

func TestSlugify(t *testing.T) {
	for _, tc := range slugVectors {
		if got := Slugify(tc.input); got != tc.slug {
			t.Errorf("Slugify(%q) = %q, want %q", tc.input, got, tc.slug)
		}
	}
}
