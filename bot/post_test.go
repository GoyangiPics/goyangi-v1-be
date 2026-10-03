package bot

import (
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/pocketbase/pocketbase/core"
)

// TestPostLayoutRender pins the agreed post shape and, crucially, which links
// are bare (unfurl) versus masked (don't).
func TestPostLayoutRender(t *testing.T) {
	full := postLayout{
		author:   "nabi",
		pings:    []string{"<@&1>", "<@&2>"},
		mirror:   "https://i.imgur.com/abc.mp4",
		source:   "https://youtu.be/xyz",
		pageLink: "https://goyangi.pics/set/260727-ive-leeseo-b4a29354",
		previews: []string{"https://cdn/a.avif", "https://cdn/b.avif"},
	}

	want := strings.Join([]string{
		"-# by nabi",
		"<@&1> <@&2>",
		"[Mirror](https://i.imgur.com/abc.mp4)",
		"[Source](https://youtu.be/xyz)",
		"https://goyangi.pics/set/260727-ive-leeseo-b4a29354",
		"https://cdn/a.avif",
		"https://cdn/b.avif",
	}, "\n")

	if got := full.render(); got != want {
		t.Errorf("render():\n got:\n%s\nwant:\n%s", got, want)
	}

	// The set link must stay bare — masking it would suppress the embed the
	// layout depends on.
	if strings.Contains(full.render(), "["+full.pageLink) {
		t.Error("the page link must not be masked")
	}

	// Anonymous drops only the byline.
	anon := full
	anon.author = ""
	if strings.Contains(anon.render(), "by ") {
		t.Errorf("anonymous post still has a byline:\n%s", anon.render())
	}
	if !strings.Contains(anon.render(), "<@&1>") {
		t.Error("anonymous post should still ping")
	}

	// Everything optional omitted: just the page link.
	minimal := postLayout{pageLink: "https://goyangi.pics/set/abc"}
	if got := minimal.render(); got != "https://goyangi.pics/set/abc" {
		t.Errorf("minimal post = %q", got)
	}
}

// TestMaskedLink covers the inputs that would otherwise render as broken
// literal markdown in the channel.
func TestMaskedLink(t *testing.T) {
	if got, want := maskedLink("Source", "https://youtu.be/xyz"), "[Source](https://youtu.be/xyz)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := maskedLink("Source", ""); got != "" {
		t.Errorf("empty input should render nothing, got %q", got)
	}
	// A bracket or space would break the masked syntax → fall back to bare.
	for _, raw := range []string{
		"https://ex.com/a(b)",
		"https://ex.com/a b",
		"not a url",
		"ftp://ex.com/x",
		"javascript:alert(1)",
	} {
		if got := maskedLink("Source", raw); got != raw {
			t.Errorf("%q should pass through bare, got %q", raw, got)
		}
	}
}

// TestPreviewsForPost checks the cover exclusion — the unfurled set embed
// already shows the newest item, so its preview must never be repeated below.
func TestPreviewsForPost(t *testing.T) {
	collection := core.NewBaseCollection("v1")
	collection.Fields.Add(&core.URLField{Name: "preview"}, &core.URLField{Name: "original"})

	record := func(id, preview string) *core.Record {
		r := core.NewRecord(collection)
		r.Id = id
		r.Set("preview", preview)
		return r
	}

	// Newest-first, so records[0] is the cover.
	set := []*core.Record{
		record("cover", "https://cdn/cover.avif"),
		record("b", "https://cdn/b.avif"),
		record("c", "https://cdn/c.avif"),
		record("d", "https://cdn/d.avif"),
	}

	for range 20 { // random pick — repeat to catch the cover leaking through
		got := previewsForPost(set, 4)
		if len(got) != 3 {
			t.Fatalf("4-item set should yield its 3 non-cover previews, got %v", got)
		}
		if contains(got, "https://cdn/cover.avif") {
			t.Fatalf("the cover preview must be excluded, got %v", got)
		}
	}

	// Capped at n, and the picks are distinct.
	got := previewsForPost(set, 2)
	if len(got) != 2 || got[0] == got[1] {
		t.Errorf("expected 2 distinct previews, got %v", got)
	}

	// A single-item set is its own cover — the set embed is the content.
	if got := previewsForPost(set[:1], 4); got != nil {
		t.Errorf("single-item set should yield no previews, got %v", got)
	}
	if got := previewsForPost(nil, 4); got != nil {
		t.Errorf("no records should yield no previews, got %v", got)
	}

	// An item whose preview never landed is skipped rather than posted blank.
	partial := []*core.Record{record("cover", "https://cdn/cover.avif"), record("b", "")}
	if got := previewsForPost(partial, 4); len(got) != 0 {
		t.Errorf("a record with no preview should be skipped, got %v", got)
	}
}

// TestSplitPingInput pins the accumulation the pings autocomplete relies on.
func TestSplitPingInput(t *testing.T) {
	tests := []struct {
		input         string
		wantCommitted string
		wantTyping    string
	}{
		{"", "", ""},
		{"IV", "", "IV"},
		{"IVE, ", "IVE", ""},
		{"IVE, lee", "IVE", "lee"},
		{"IVE, Leeseo [IVE], won", "IVE, Leeseo [IVE]", "won"},
		{"IVE ,", "IVE", ""},
	}
	for _, tt := range tests {
		committed, typing := splitPingInput(tt.input)
		if committed != tt.wantCommitted || typing != tt.wantTyping {
			t.Errorf("splitPingInput(%q) = (%q, %q), want (%q, %q)",
				tt.input, committed, typing, tt.wantCommitted, tt.wantTyping)
		}
	}
}

// TestPingChoices checks that suggestions append to what's already chosen, skip
// what's already there, and never offer @everyone.
func TestPingChoices(t *testing.T) {
	roles := []discord.Role{
		{Name: "@everyone"},
		{Name: "IVE"},
		{Name: "Leeseo [IVE]"},
		{Name: "Wonyoung [IVE]"},
	}

	// Fresh input: plain role names.
	got := choiceValues(pingChoices(roles, ""))
	for _, want := range []string{"IVE", "Leeseo [IVE]", "Wonyoung [IVE]"} {
		if !contains(got, want) {
			t.Errorf("expected %q among %v", want, got)
		}
	}
	if contains(got, "@everyone") {
		t.Errorf("@everyone must never be suggested, got %v", got)
	}

	// After one pick: suggestions extend the list and drop the taken one.
	got = choiceValues(pingChoices(roles, "IVE, lee"))
	if len(got) != 1 || got[0] != "IVE, Leeseo [IVE]" {
		t.Errorf("accumulating choice = %v, want [\"IVE, Leeseo [IVE]\"]", got)
	}
	if contains(choiceValues(pingChoices(roles, "IVE, ")), "IVE, IVE") {
		t.Error("an already-chosen role must not be offered again")
	}

	// Filtering is a case-insensitive substring match.
	if got := choiceValues(pingChoices(roles, "wonyo")); len(got) != 1 || got[0] != "Wonyoung [IVE]" {
		t.Errorf("substring match = %v", got)
	}

	// An accumulated value over Discord's 100-char choice cap is dropped rather
	// than sent and rejected. Exactly 100 is still offered.
	atCap := strings.Repeat("x", maxChoiceValueLength-len(", IVE")) // → exactly 100
	if got := choiceValues(pingChoices(roles, atCap+", IV")); !contains(got, atCap+", IVE") {
		t.Errorf("a value of exactly %d chars should still be offered, got %v", maxChoiceValueLength, got)
	}
	overCap := strings.Repeat("x", maxChoiceValueLength)
	if got := choiceValues(pingChoices(roles, overCap+", IV")); len(got) != 0 {
		t.Errorf("over-long value should be skipped, got %v", got)
	}
}

func choiceValues(choices []discord.AutocompleteChoice) []string {
	out := make([]string, 0, len(choices))
	for _, choice := range choices {
		if s, ok := choice.(discord.AutocompleteChoiceString); ok {
			out = append(out, s.Value)
		}
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// TestAllowedRoleMentions guards the blast radius: a post may only ping the
// roles it names, never @everyone, even if the text somehow contains it.
func TestAllowedRoleMentions(t *testing.T) {
	am := allowedRoleMentions(nil)
	if am.Parse == nil {
		t.Fatal("Parse must be an explicit empty slice, not nil — nil marshals to null and Discord then parses every mention in the content")
	}
	if len(am.Parse) != 0 {
		t.Errorf("Parse must be empty, got %v", am.Parse)
	}
}

// TestQuoteList covers the error text shown when a ping doesn't resolve.
func TestQuoteList(t *testing.T) {
	if got, want := quoteList([]string{"Nobody"}), "`Nobody`"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := quoteList([]string{"A", "B"}), "`A` or `B`"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := quoteList([]string{"A", "B", "C"}), "`A`, `B` or `C`"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
