package hooks

import (
	"net/http"
	"reflect"
	"slices"
	"testing"

	"github.com/pocketbase/pocketbase/core"
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

// A merge has to find every set that lists the source, through the real filter
// on the real schema: contents_sets.uploader is a multi relation, and a filter
// that silently matches nothing there moves no set and lets the guard delete a
// source that is still listed.
func TestMergeUploadersRepointsTheSourcesSets(t *testing.T) {
	f := newFixture(t)
	uploaders, _ := f.app.FindCollectionByNameOrId("uploaders")
	dupe := core.NewRecord(uploaders)
	dupe.Set("name", "u1 on discord")
	mustSave(t, f.app, dupe)
	f.up["dupe"] = dupe.Id

	// A set of the duplicate's own posts, and one shared with u2 that lists the
	// duplicate without holding a post of theirs — a list from before it was
	// derived, which no post move re-derives. The first goes to the target; the
	// second only loses the duplicate, as the derived list would.
	own := f.set(t, "dupe")
	f.post(t, "dupe", own)
	stale := f.set(t, "u2")
	f.post(t, "u2", stale) // derives [u2]; the stale list is written after
	staleSet := f.record(t, "contents_sets", stale)
	staleSet.Set("uploader", []string{f.up["u2"], dupe.Id})
	mustSave(t, f.app, staleSet)

	f.do(t, "merge", http.MethodPost, "/api/admin/uploaders/merge",
		`{"target":"`+f.up["u1"]+`","sources":["`+dupe.Id+`"]}`, "admin", 200, `"movedContent":1`, `"movedSets":2`)

	if f.record(t, "uploaders", dupe.Id) != nil {
		t.Error("the source survived the merge")
	}
	for set, want := range map[string][]string{
		own:   {f.up["u1"]},
		stale: {f.up["u2"]},
	} {
		if got := f.record(t, "contents_sets", set).GetStringSlice("uploader"); !slices.Equal(got, want) {
			t.Errorf("set %s uploaders = %v, want %v", set, got, want)
		}
	}
}

// The step-4 guard's query on its own: it must see a source a set still lists,
// alone or beside a co-uploader, or it deletes a referenced record.
func TestSetsListingUploader(t *testing.T) {
	f := newFixture(t)
	solo := f.set(t, "u1")
	shared := f.set(t, "u2", "u1")
	f.set(t, "u2")

	got, err := setsListingUploader(f.app, f.up["u1"], 0)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.Id)
	}
	slices.Sort(ids)
	want := []string{solo, shared}
	slices.Sort(want)
	if !slices.Equal(ids, want) {
		t.Errorf("sets listing u1 = %v, want %v", ids, want)
	}
}
