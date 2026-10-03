package hooks

import (
	"reflect"
	"testing"
)

func TestUnionStrings(t *testing.T) {
	tests := []struct {
		name string
		base []string
		add  []string
		want []string
	}{
		{"into empty", nil, []string{"a", "b"}, []string{"a", "b"}},
		{"nothing to add", []string{"a"}, nil, []string{"a"}},
		{"skips duplicates already in base", []string{"a", "b"}, []string{"b", "c"}, []string{"a", "b", "c"}},
		{"skips duplicates within add", []string{"a"}, []string{"b", "b"}, []string{"a", "b"}},
		{"drops empty strings", []string{"a"}, []string{"", "b", ""}, []string{"a", "b"}},
		{"preserves base order", []string{"z", "y"}, []string{"x"}, []string{"z", "y", "x"}},
		{"fully overlapping is a no-op", []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Copy: unionStrings appends, so a shared backing array would let one
			// case scribble on another's fixture.
			base := append([]string(nil), tt.base...)
			got := unionStrings(base, tt.add...)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("unionStrings(%v, %v) = %v, want %v", tt.base, tt.add, got, tt.want)
			}
		})
	}
}

func TestStripDatePrefix(t *testing.T) {
	tests := []struct{ in, want string }{
		// createSetRecord builds set titles as "YYMMDD <title>"; content titles
		// have no such prefix, so propagating one verbatim would leak it.
		{"260727 Leeseo fancam", "Leeseo fancam"},
		{"060102 x", "x"},
		// Not a date prefix — must be left alone.
		{"Leeseo fancam", "Leeseo fancam"},
		{"26072 Leeseo", "26072 Leeseo"},     // five digits
		{"2607271 Leeseo", "2607271 Leeseo"}, // seven digits
		{"26072a Leeseo", "26072a Leeseo"},   // not all digits
		{"260727", "260727"},                 // no space, nothing follows
		{"", ""},
		// A title that is only the prefix plus a space yields the empty remainder.
		{"260727 ", ""},
	}

	for _, tt := range tests {
		if got := stripDatePrefix(tt.in); got != tt.want {
			t.Errorf("stripDatePrefix(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPropagatableSetFieldsExcludesPipelineAndProvenance(t *testing.T) {
	// Guards the allowlist against a well-meaning addition. Propagating any of
	// these would either overwrite what the encode pipeline owns or rewrite
	// provenance the user isn't permitted to set.
	for _, field := range []string{
		"file", "original", "preview", "static", "sd", "preview_format",
		"discord", "mirror", "origin",
		"views", "likes", "collections", "labels",
		"set", "tag",
		"filetype", "source", "interpolate", "interpolate_mode",
	} {
		if propagatableSetFields[field] {
			t.Errorf("%q must not be propagatable", field)
		}
	}

	for _, field := range []string{"title", "idol", "group", "date", "uploader"} {
		if !propagatableSetFields[field] {
			t.Errorf("%q should be propagatable", field)
		}
	}
}
