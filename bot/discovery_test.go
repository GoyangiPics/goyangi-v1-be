package bot

import (
	"strings"
	"testing"
)

// TestResolveLink pins the per-format field precedence, including the
// fallbacks that keep a half-processed record usable.
func TestResolveLink(t *testing.T) {
	// get serves a record's fields; an absent field reads as empty, exactly
	// like core.Record.GetString.
	get := func(fields map[string]string) func(string) string {
		return func(field string) string { return fields[field] }
	}

	full := get(map[string]string{
		"original": "https://cdn/clip.mp4",
		"preview":  "https://cdn/clip.avif",
		"static":   "https://cdn/clip-static.avif",
		"mirror":   "https://i.imgur.com/clip.mp4",
	})
	for format, want := range map[string]string{
		formatMP4:     "https://cdn/clip.mp4",
		formatPreview: "https://cdn/clip.avif",
		formatStatic:  "https://cdn/clip-static.avif",
		// An unknown format behaves as "preview" — the default.
		"nonsense": "https://cdn/clip.avif",
	} {
		if got := resolveLink(format, full); got != want {
			t.Errorf("%s: got %q, want %q", format, got, want)
		}
	}

	// The imgur mirror is the last resort for every format.
	mirrorOnly := get(map[string]string{"mirror": "https://i.imgur.com/clip.mp4"})
	for _, format := range []string{formatMP4, formatPreview, formatStatic} {
		if got := resolveLink(format, mirrorOnly); got != "https://i.imgur.com/clip.mp4" {
			t.Errorf("%s mirror fallback: got %q", format, got)
		}
	}

	// A gif whose preview encode failed still yields its MP4 under "preview".
	noPreview := get(map[string]string{"original": "https://cdn/clip.mp4"})
	if got := resolveLink(formatPreview, noPreview); got != "https://cdn/clip.mp4" {
		t.Errorf("preview should fall back to the mp4, got %q", got)
	}

	// R2 hasn't run yet — nothing to link, and mediaLink falls back to the
	// PocketBase-hosted file instead.
	if got := resolveLink(formatMP4, get(nil)); got != "" {
		t.Errorf("empty record should yield no link, got %q", got)
	}
}

// TestDiscordSafeTitle covers the characters that would break the masked link
// a title is rendered into.
func TestDiscordSafeTitle(t *testing.T) {
	for input, want := range map[string]string{
		"251203 Yuna":      "251203 Yuna",
		"  padded  ":       "padded",
		"a [b] c":          "a (b) c",
		"line\nbreak":      "line break",
		"":                 "untitled",
		"   ":              "untitled",
		"]injected](evil)": ")injected)(evil)",
	} {
		if got := discordSafeTitle(input); got != want {
			t.Errorf("discordSafeTitle(%q) = %q, want %q", input, got, want)
		}
	}
}

// TestPeriodStart checks the bounded/unbounded split — "all" is the only value
// that must report no cutoff, since that's what drops the filter clause.
func TestPeriodStart(t *testing.T) {
	if _, bounded := periodStart("all"); bounded {
		t.Error(`"all" must be unbounded`)
	}
	if _, bounded := periodStart("nonsense"); bounded {
		t.Error("an unknown period must be unbounded rather than a zero cutoff")
	}
	for _, c := range periodChoices {
		if c.Value == "all" {
			continue
		}
		if _, bounded := periodStart(c.Value); !bounded {
			t.Errorf("period %q is offered as a choice but has no cutoff", c.Value)
		}
	}
}

// TestFindIdols covers the reason `group` could become optional: a bare name
// can match several idols, and all of them have to come back.
func TestFindIdols(t *testing.T) {
	dir := &directory{
		groups: []groupEntry{{id: "g_itzy", name: "ITZY"}, {id: "g_aespa", name: "aespa"}},
		idols: []idolEntry{
			{id: "i_yuna_itzy", name: "Yuna", groupID: "g_itzy", groupName: "ITZY"},
			{id: "i_yuna_aespa", name: "Yuna", groupID: "g_aespa", groupName: "aespa"},
			{id: "i_lia", name: "Lia", groupID: "g_itzy", groupName: "ITZY"},
		},
	}

	if got := findIdols(dir, "yuna", ""); len(got) != 2 {
		t.Errorf("an ambiguous name should match every idol: got %d", len(got))
	}
	got := findIdols(dir, "Yuna", "g_itzy")
	if len(got) != 1 || got[0].id != "i_yuna_itzy" {
		t.Errorf("group should disambiguate: got %#v", got)
	}
	// An autocomplete choice sends the record id, which must resolve exactly.
	if got := findIdols(dir, "i_yuna_aespa", ""); len(got) != 1 || got[0].groupName != "aespa" {
		t.Errorf("id lookup: got %#v", got)
	}
	// ...but not when it contradicts the group that's also filled in.
	if got := findIdols(dir, "i_yuna_aespa", "g_itzy"); len(got) != 0 {
		t.Errorf("id outside the chosen group should not match: got %#v", got)
	}
	if got := findIdols(dir, "Nobody", ""); len(got) != 0 {
		t.Errorf("unknown name should match nothing: got %#v", got)
	}

	if g, ok := findGroup(dir, "itzy"); !ok || g.id != "g_itzy" {
		t.Errorf("group name lookup is case-insensitive: got %#v (ok=%v)", g, ok)
	}
	if g, ok := findGroup(dir, "g_aespa"); !ok || g.name != "aespa" {
		t.Errorf("group id lookup: got %#v (ok=%v)", g, ok)
	}
	if _, ok := findGroup(dir, ""); ok {
		t.Error("an empty group option must not resolve")
	}
}

// TestIdolChoiceLabel keeps the autocomplete label inside Discord's 100-char
// cap and shows the group, which is what makes duplicate names pickable.
func TestIdolChoiceLabel(t *testing.T) {
	got := idolChoiceLabel(idolEntry{name: "Yuna", groupName: "ITZY"})
	if !strings.Contains(got, "Yuna") || !strings.Contains(got, "ITZY") {
		t.Errorf("label should name both idol and group: %q", got)
	}
	if got := idolChoiceLabel(idolEntry{name: "Solo"}); got != "Solo" {
		t.Errorf("groupless idol: got %q", got)
	}
}
