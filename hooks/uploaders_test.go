package hooks

import (
	"reflect"
	"testing"
)

func TestSplitTrimStrings(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"   ", []string{}},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{" a , b ", []string{"a", "b"}},
		// Empty entries dropped, so a trailing comma (easy to leave behind when
		// editing the field by hand in the admin UI) costs nothing.
		{"a,,b,", []string{"a", "b"}},
		{",", []string{}},
	}
	for _, tt := range tests {
		if got := splitTrimStrings(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitTrimStrings(%q) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
}

func TestContainsFold(t *testing.T) {
	list := []string{"Nabi", "goyangi_bot"}
	for _, want := range []string{"Nabi", "nabi", "NABI", "goyangi_bot"} {
		if !containsFold(list, want) {
			t.Errorf("containsFold(%v, %q) = false, want true", list, want)
		}
	}
	for _, want := range []string{"chae", "", "other"} {
		if containsFold(list, want) {
			t.Errorf("containsFold(%v, %q) = true, want false", list, want)
		}
	}
}

func TestAbsorbAliases(t *testing.T) {
	tests := []struct {
		name        string
		targetName  string
		targetAlias string
		sources     []uploaderNames
		wantAliases string
		wantAdded   []string
	}{
		{
			// The case this exists for: the Discord username becomes an alias of
			// the site uploader, so the next ingest resolves onto it instead of
			// minting the duplicate again.
			name:        "adds a source name to an empty alias list",
			targetName:  "nabi",
			sources:     []uploaderNames{{name: "nabi_pics"}},
			wantAliases: "nabi_pics",
			wantAdded:   []string{"nabi_pics"},
		},
		{
			name:        "appends to existing aliases, preserving order",
			targetName:  "nabi",
			targetAlias: "kitty",
			sources:     []uploaderNames{{name: "nabi_pics"}},
			wantAliases: "kitty, nabi_pics",
			wantAdded:   []string{"nabi_pics"},
		},
		{
			name:        "carries the source's own aliases across too",
			targetName:  "nabi",
			sources:     []uploaderNames{{name: "nabi_pics", aliases: "cpics, nabi-dc"}},
			wantAliases: "nabi_pics, cpics, nabi-dc",
			wantAdded:   []string{"nabi_pics", "cpics", "nabi-dc"},
		},
		{
			name:        "never aliases the target to its own name, whatever the case",
			targetName:  "Nabi",
			sources:     []uploaderNames{{name: "nabi"}, {name: "NABI"}},
			wantAliases: "",
			wantAdded:   nil,
		},
		{
			name:        "dedupes case-insensitively against existing aliases",
			targetName:  "nabi",
			targetAlias: "Nabi_Pics",
			sources:     []uploaderNames{{name: "nabi_pics"}},
			wantAliases: "Nabi_Pics",
			wantAdded:   nil,
		},
		{
			name:        "dedupes across several sources",
			targetName:  "nabi",
			sources:     []uploaderNames{{name: "dupe"}, {name: "dupe"}},
			wantAliases: "dupe",
			wantAdded:   []string{"dupe"},
		},
		{
			// A comma would be read as two aliases by every splitTrim on the read
			// side, so a name carrying one is skipped rather than corrupting the
			// list. Only reachable through a NAME — aliases were comma-split to
			// get here.
			name:        "refuses a name containing a comma",
			targetName:  "nabi",
			sources:     []uploaderNames{{name: "a,b"}, {name: "safe"}},
			wantAliases: "safe",
			wantAdded:   []string{"safe"},
		},
		{
			name:        "ignores blank names",
			targetName:  "nabi",
			sources:     []uploaderNames{{name: "   "}, {name: ""}},
			wantAliases: "",
			wantAdded:   nil,
		},
		{
			name:        "no sources is a no-op that preserves the list",
			targetName:  "nabi",
			targetAlias: "kitty, cat",
			sources:     nil,
			wantAliases: "kitty, cat",
			wantAdded:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotAliases, gotAdded := absorbAliases(tt.targetName, tt.targetAlias, tt.sources)
			if gotAliases != tt.wantAliases {
				t.Errorf("aliases = %q, want %q", gotAliases, tt.wantAliases)
			}
			if !reflect.DeepEqual(gotAdded, tt.wantAdded) {
				t.Errorf("added = %#v, want %#v", gotAdded, tt.wantAdded)
			}
		})
	}
}
