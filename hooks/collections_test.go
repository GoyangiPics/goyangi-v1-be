package hooks

import "testing"

// addedStrings is the whole decision: only newly-added collection ids get an
// ownership check, so getting this wrong either lets an addition through
// unchecked or blocks an edit that changed nothing.
func TestAddedStrings(t *testing.T) {
	tests := []struct {
		name string
		prev []string
		next []string
		want []string
	}{
		{"create — everything is new", nil, []string{"c1", "c2"}, []string{"c1", "c2"}},
		{"unchanged", []string{"c1"}, []string{"c1"}, nil},
		{"one added", []string{"c1"}, []string{"c1", "c2"}, []string{"c2"}},
		// A removal must not be reported as an addition, or removing your content
		// from someone else's collection would be rejected.
		{"only removed", []string{"c1", "c2"}, []string{"c1"}, nil},
		{"removed and added at once", []string{"c1"}, []string{"c2"}, []string{"c2"}},
		{"cleared entirely", []string{"c1", "c2"}, nil, nil},
		{"order is irrelevant", []string{"c1", "c2"}, []string{"c2", "c1"}, nil},
		{"blank ids are ignored", []string{"c1"}, []string{"c1", ""}, nil},
		{"duplicates in next collapse", nil, []string{"c1", "c1"}, []string{"c1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := addedStrings(tt.prev, tt.next)
			if len(got) != len(tt.want) {
				t.Fatalf("addedStrings(%v, %v) = %v, want %v", tt.prev, tt.next, got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
