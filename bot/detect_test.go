package bot

import (
	"strings"
	"testing"
)

// detectDirectory deliberately includes the awkward cases from the real data:
// a two-word name people shorten ("Kwon Eunbi" → "Eunbi"), a group people
// abbreviate ("Rocket Punch" → "RcPc"), a three-letter name that collides with
// ordinary English ("Ian"), and one name shared by two groups ("Yuna").
func detectDirectory() *directory {
	return &directory{
		groups: []groupEntry{
			{id: "g1", name: "IVE"},
			{id: "g2", name: "Rocket Punch", aliases: []string{"RcPc"}},
			{id: "g3", name: "ITZY"},
			{id: "g4", name: "AESPA"},
		},
		idols: []idolEntry{
			{id: "i1", name: "Yujin", groupID: "g1", groupName: "IVE"},
			{id: "i2", name: "Kwon Eunbi", aliases: []string{"Eunbi"}, groupID: "g2", groupName: "Rocket Punch"},
			{id: "i3", name: "Ian", groupID: "g1", groupName: "IVE"},
			{id: "i4", name: "Yuna", groupID: "g3", groupName: "ITZY"},
			{id: "i5", name: "Yuna", groupID: "g4", groupName: "AESPA"},
		},
	}
}

func TestDetectSubjects(t *testing.T) {
	dir := detectDirectory()

	t.Run("a plain name attributes to it and its group", func(t *testing.T) {
		got, ok := detectSubjects(dir, "yujin looking great today")
		if !ok {
			t.Fatal("expected a detection")
		}
		if strings.Join(got.idolNames, ",") != "Yujin" {
			t.Errorf("idols = %v", got.idolNames)
		}
		if strings.Join(got.groupNames, ",") != "IVE" {
			t.Errorf("groups = %v", got.groupNames)
		}
	})

	t.Run("an alias resolves to the canonical name", func(t *testing.T) {
		got, ok := detectSubjects(dir, "eunbi stage cut")
		if !ok {
			t.Fatal("expected a detection")
		}
		if got.idolNames[0] != "Kwon Eunbi" {
			t.Errorf("idols = %v, want the canonical name", got.idolNames)
		}
		if got.groupNames[0] != "Rocket Punch" {
			t.Errorf("groups = %v", got.groupNames)
		}
	})

	t.Run("a group alias counts as naming the group", func(t *testing.T) {
		got, ok := detectSubjects(dir, "rcpc comeback")
		if ok {
			t.Fatalf("a group alone must not attribute: %+v", got)
		}
	})

	t.Run("matching is on word boundaries", func(t *testing.T) {
		for _, content := range []string{
			"reinstalling the app",    // no idol at all
			"ianuary was a while ago", // "Ian" inside a longer word
			"guyujins",                // "Yujin" inside a longer word
		} {
			if got, ok := detectSubjects(dir, content); ok {
				t.Errorf("detectSubjects(%q) matched %+v, want nothing", content, got)
			}
		}
	})

	t.Run("names inside links are ignored", func(t *testing.T) {
		// The slug names an idol, the message says nothing. Attribution has to
		// come from what a person wrote.
		if got, ok := detectSubjects(dir, "https://imgur.com/kwon-eunbi-260731-AbCdE12"); ok {
			t.Errorf("matched %+v from a URL alone", got)
		}
	})

	t.Run("an ambiguous name needs its group named too", func(t *testing.T) {
		if got, ok := detectSubjects(dir, "yuna today"); ok {
			t.Errorf("matched %+v, want a refusal — Yuna is in two groups", got)
		}
		got, ok := detectSubjects(dir, "yuna from itzy today")
		if !ok {
			t.Fatal("naming the group should disambiguate")
		}
		if len(got.idolNames) != 1 || got.idolNames[0] != "Yuna" {
			t.Errorf("idols = %v", got.idolNames)
		}
		if strings.Join(got.groupNames, ",") != "ITZY" {
			t.Errorf("groups = %v, want only the named one", got.groupNames)
		}
	})

	t.Run("several idols in one message all attribute", func(t *testing.T) {
		got, ok := detectSubjects(dir, "yujin and eunbi")
		if !ok {
			t.Fatal("expected a detection")
		}
		if len(got.idolNames) != 2 {
			t.Errorf("idols = %v, want both", got.idolNames)
		}
	})

	t.Run("nothing recognisable is not a detection", func(t *testing.T) {
		if _, ok := detectSubjects(dir, "anyone got the fancam from yesterday"); ok {
			t.Error("matched on a message naming nobody")
		}
	})

	t.Run("the matched strings are recorded for the audit log", func(t *testing.T) {
		got, _ := detectSubjects(dir, "eunbi stage cut")
		if strings.Join(got.matched, ",") != "Eunbi" {
			t.Errorf("matched = %v, want the alias that actually fired", got.matched)
		}
	})
}

func TestDetectStopwordsSuppress(t *testing.T) {
	dir := detectDirectory()

	orig := detectStopwords
	detectStopwords = map[string]bool{"ian": true}
	t.Cleanup(func() { detectStopwords = orig })

	if got, ok := detectSubjects(dir, "ian was there"); ok {
		t.Errorf("matched %+v despite the stopword", got)
	}
	// Everything else still works.
	if _, ok := detectSubjects(dir, "yujin was there"); !ok {
		t.Error("a stopword suppressed an unrelated name")
	}
}

func TestContainsWord(t *testing.T) {
	cases := []struct {
		haystack, needle string
		want             bool
	}{
		{" yujin fancam ", "yujin", true},
		{" a yujin, nice ", "yujin", true},
		{" (yujin) ", "yujin", true},
		{" yujin's stage ", "yujin", true},
		{" guyujins ", "yujin", false},
		{" yujins ", "yujin", false},
		{" reinstall ", "rei", false},
		{" rei ", "rei", true},
		{" ", "rei", false},
	}
	for _, tt := range cases {
		if got := containsWord(tt.haystack, tt.needle); got != tt.want {
			t.Errorf("containsWord(%q, %q) = %v, want %v", tt.haystack, tt.needle, got, tt.want)
		}
	}
}

// TestDetectSubjectsRealScenarios pins the three posting styles users asked
// about, in the words they were asked in. `@ifeye` is a GROUP role — it carries
// no idol, so scenario 1 only works because the role name is scanned alongside
// the message text.
func TestDetectSubjectsRealScenarios(t *testing.T) {
	dir := &directory{
		groups: []groupEntry{{id: "g1", name: "ifeye"}},
		idols: []idolEntry{
			{id: "i1", name: "Rahee", groupID: "g1", groupName: "ifeye"},
		},
	}

	cases := []struct {
		name    string
		content string // message text plus any pinged role names, as textDetection joins them
	}{
		{"1. group ping plus the idol in text", "Rahee\nifeye"},
		{"2. no ping, Idol [Group] in text", "Rahee [ifeye]"},
		{"3. no ping, just the idol name", "Rahee"},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := detectSubjects(dir, tt.content)
			if !ok {
				t.Fatalf("detectSubjects(%q) found nothing", tt.content)
			}
			if len(got.idolNames) != 1 || got.idolNames[0] != "Rahee" {
				t.Errorf("idols = %v, want [Rahee]", got.idolNames)
			}
			if len(got.groupNames) != 1 || got.groupNames[0] != "ifeye" {
				t.Errorf("groups = %v, want [ifeye]", got.groupNames)
			}
		})
	}
}
