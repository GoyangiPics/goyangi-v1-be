package bot

import (
	"strings"
	"testing"

	"github.com/disgoorg/snowflake/v2"
)

func TestParseMessageLink(t *testing.T) {
	const (
		guild   = "1234567890123456789"
		channel = "2234567890123456789"
		message = "3234567890123456789"
	)

	ok := []struct {
		name string
		link string
	}{
		{"plain", "https://discord.com/channels/" + guild + "/" + channel + "/" + message},
		{"ptb build", "https://ptb.discord.com/channels/" + guild + "/" + channel + "/" + message},
		{"canary build", "https://canary.discord.com/channels/" + guild + "/" + channel + "/" + message},
		// Old links are still served from discordapp.com.
		{"legacy host", "https://discordapp.com/channels/" + guild + "/" + channel + "/" + message},
		{"http", "http://discord.com/channels/" + guild + "/" + channel + "/" + message},
		// The value arrives from a pasted command option, so tolerate the mess.
		{"leading and trailing space", "  https://discord.com/channels/" + guild + "/" + channel + "/" + message + "  "},
		{"trailing slash", "https://discord.com/channels/" + guild + "/" + channel + "/" + message + "/"},
		{"query string", "https://discord.com/channels/" + guild + "/" + channel + "/" + message + "?jump=1"},
		{"surrounded by text", "look at this https://discord.com/channels/" + guild + "/" + channel + "/" + message + " nice"},
	}

	for _, tt := range ok {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := parseMessageLink(tt.link)
			if err != nil {
				t.Fatalf("parseMessageLink(%q) errored: %v", tt.link, err)
			}
			if ref.guildID.String() != guild {
				t.Errorf("guild = %s, want %s", ref.guildID, guild)
			}
			if ref.channelID.String() != channel {
				t.Errorf("channel = %s, want %s", ref.channelID, channel)
			}
			if ref.messageID.String() != message {
				t.Errorf("message = %s, want %s", ref.messageID, message)
			}
		})
	}

	bad := []struct {
		name string
		link string
	}{
		// A DM has no guild message URL to stamp onto the records, so it is
		// matched only to be rejected with a useful message.
		{"dm", "https://discord.com/channels/@me/" + channel + "/" + message},
		{"empty", ""},
		{"whitespace", "   "},
		{"not a link", "260727-ive-leeseo-b4a29354"},
		// A goyangi content link is the other thing someone might paste here.
		{"goyangi single link", "https://goyangi.pics/single/260727-ive-leeseo-b4a29354"},
		{"channel link, no message", "https://discord.com/channels/" + guild + "/" + channel},
		{"wrong host", "https://example.com/channels/1/2/3"},
		// Prefix-of-host must not match.
		{"lookalike host", "https://notdiscord.com/channels/1/2/3"},
	}

	for _, tt := range bad {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			if _, err := parseMessageLink(tt.link); err == nil {
				t.Fatalf("parseMessageLink(%q) should have errored", tt.link)
			}
		})
	}
}

func TestParseMessageLinkRejectsNonNumericIDs(t *testing.T) {
	// The regexp only matches digits, so these fail at the match stage rather
	// than in snowflake.Parse — assert the behaviour, not the mechanism.
	for _, link := range []string{
		"https://discord.com/channels/abc/222/333",
		"https://discord.com/channels/111/abc/333",
		"https://discord.com/channels/111/222/abc",
	} {
		if _, err := parseMessageLink(link); err == nil {
			t.Errorf("parseMessageLink(%q) should have errored", link)
		}
	}
}

// testDirectory is the fixture the multi-pick helpers resolve against. Ids are
// 15 characters like PocketBase's, so the value-length guard is exercised
// realistically rather than against short stand-ins.
func testDirectory() *directory {
	return &directory{
		groups: []groupEntry{
			{id: "grp000000000001", name: "IVE"},
			{id: "grp000000000002", name: "LE SSERAFIM"},
		},
		idols: []idolEntry{
			{id: "idl000000000001", name: "Yujin", groupID: "grp000000000001", groupName: "IVE"},
			{id: "idl000000000002", name: "Gaeul", groupID: "grp000000000001", groupName: "IVE"},
			{id: "idl000000000003", name: "Chae Won", groupID: "grp000000000002", groupName: "LE SSERAFIM"},
		},
		tagNames: []string{"fancam", "4k", "stage"},
	}
}

func TestMultiPickState(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantChosen []string
		wantNeedle string
	}{
		{"empty", "", nil, ""},
		{"typing the first entry", "yuj", nil, "yuj"},
		// The trailing comma is the contract: every value the autocomplete hands
		// back ends with one, so its presence means "that entry is committed".
		{"one committed pick", "idl000000000001,", []string{"idl000000000001"}, ""},
		{"typing after a pick", "idl000000000001,gae", []string{"idl000000000001"}, "gae"},
		{"two committed picks", "idl000000000001,idl000000000002,", []string{"idl000000000001", "idl000000000002"}, ""},
		{"needle is lowercased", "YUJ", nil, "yuj"},
		{"surrounding space", "  idl000000000001,  ", []string{"idl000000000001"}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chosen, needle := multiPickState(tt.raw)
			if needle != tt.wantNeedle {
				t.Errorf("needle = %q, want %q", needle, tt.wantNeedle)
			}
			if len(chosen) != len(tt.wantChosen) {
				t.Fatalf("chosen = %v, want %v", chosen, tt.wantChosen)
			}
			for i := range tt.wantChosen {
				if chosen[i] != tt.wantChosen[i] {
					t.Errorf("chosen[%d] = %q, want %q", i, chosen[i], tt.wantChosen[i])
				}
			}
		})
	}
}

func TestMultiPickChoicesAppend(t *testing.T) {
	dir := testDirectory()

	t.Run("first pick offers every idol", func(t *testing.T) {
		choices := multiPickChoices(dir, "idols", "")
		if len(choices) != 3 {
			t.Fatalf("got %d choices, want 3", len(choices))
		}
	})

	t.Run("value appends to what is already picked", func(t *testing.T) {
		choices := multiPickChoices(dir, "idols", "idl000000000001,gae")
		if len(choices) != 1 {
			t.Fatalf("got %d choices, want 1", len(choices))
		}
		got := choiceValues(choices)[0]
		want := "idl000000000001,idl000000000002,"
		if got != want {
			t.Errorf("value = %q, want %q", got, want)
		}
	})

	t.Run("already-picked entries are not offered again", func(t *testing.T) {
		for _, value := range choiceValues(multiPickChoices(dir, "idols", "idl000000000001,")) {
			if value == "idl000000000001,idl000000000001," {
				t.Fatal("offered a duplicate pick")
			}
		}
	})

	t.Run("never exceeds Discord's value limit", func(t *testing.T) {
		// Six 15-char ids plus separators is already 96 characters, so a seventh
		// cannot fit and must be dropped rather than offered and rejected.
		full := ""
		for i := 0; i < 6; i++ {
			full += "idl00000000000" + string(rune('1'+i)) + ","
		}
		for _, value := range choiceValues(multiPickChoices(dir, "idols", full)) {
			if len(value) > autocompleteValueLimit {
				t.Errorf("value %q is %d chars, over the %d limit", value, len(value), autocompleteValueLimit)
			}
		}
	})

	// The bug: /reupload failed whenever more than one idol was picked, with
	// "I don't know: <first idol> — <group>."
	//
	// Discord's input box keeps the DISPLAYED text of a chosen suggestion, not
	// the value behind it. Typing a comma to add a second idol therefore hands
	// the label back as the option value, and the old code appended the next id
	// to that label verbatim — building "Yujin — IVE,idl000000000002," and
	// submitting a label the resolver had never been taught to read.
	t.Run("a label echoed back is re-emitted as its id", func(t *testing.T) {
		choices := multiPickChoices(dir, "idols", "Yujin — IVE,gae")
		if len(choices) != 1 {
			t.Fatalf("got %d choices, want 1", len(choices))
		}
		got := choiceValues(choices)[0]
		want := "idl000000000001,idl000000000002,"
		if got != want {
			t.Errorf("value = %q, want %q", got, want)
		}
	})

	t.Run("an idol picked under its label is not offered again", func(t *testing.T) {
		for _, value := range choiceValues(multiPickChoices(dir, "idols", "Yujin — IVE,")) {
			if strings.Contains(value, "idl000000000001,idl000000000001,") {
				t.Fatal("offered a duplicate pick")
			}
		}
	})

	t.Run("heals a whole run of labels at once", func(t *testing.T) {
		// Every pick after the first echoes as a label, so by the third the
		// value is nearly all labels — and long enough to threaten the 100-char
		// cap that ids were sized against.
		choices := multiPickChoices(dir, "idols", "Yujin — IVE,Gaeul — IVE,chae")
		if len(choices) != 1 {
			t.Fatalf("got %d choices, want 1", len(choices))
		}
		got := choiceValues(choices)[0]
		want := "idl000000000001,idl000000000002,idl000000000003,"
		if got != want {
			t.Errorf("value = %q, want %q", got, want)
		}
	})

	t.Run("tags pick by name", func(t *testing.T) {
		choices := multiPickChoices(dir, "tags", "fan")
		if len(choices) != 1 {
			t.Fatalf("got %d choices, want 1", len(choices))
		}
		if got := choiceValues(choices)[0]; got != "fancam," {
			t.Errorf("value = %q, want %q", got, "fancam,")
		}
	})
}

func TestResolveReuploadIdols(t *testing.T) {
	dir := testDirectory()

	t.Run("groups follow from the idols", func(t *testing.T) {
		idols, groups, unknown := resolveReuploadIdols(dir, "idl000000000001,idl000000000003,")
		if len(unknown) != 0 {
			t.Fatalf("unknown = %v", unknown)
		}
		if got := strings.Join(idols, ", "); got != "Yujin, Chae Won" {
			t.Errorf("idols = %q", got)
		}
		if got := strings.Join(groups, ", "); got != "IVE, LE SSERAFIM" {
			t.Errorf("groups = %q", got)
		}
	})

	t.Run("a shared group appears once", func(t *testing.T) {
		_, groups, _ := resolveReuploadIdols(dir, "idl000000000001,idl000000000002,")
		if len(groups) != 1 || groups[0] != "IVE" {
			t.Errorf("groups = %v, want [IVE]", groups)
		}
	})

	t.Run("hand-typed names still resolve", func(t *testing.T) {
		idols, groups, unknown := resolveReuploadIdols(dir, "Yujin")
		if len(unknown) != 0 || len(idols) != 1 || idols[0] != "Yujin" || groups[0] != "IVE" {
			t.Errorf("idols=%v groups=%v unknown=%v", idols, groups, unknown)
		}
	})

	// The exact shape the bug submitted: the first pick echoed back as its
	// label, the last one as an id. Submitting without picking a further idol
	// leaves it that way, so the resolver has to read both forms.
	t.Run("accepts the label form the input box echoes back", func(t *testing.T) {
		idols, groups, unknown := resolveReuploadIdols(dir, "Yujin — IVE,idl000000000002,")
		if len(unknown) != 0 {
			t.Fatalf("unknown = %v, want none", unknown)
		}
		if got := strings.Join(idols, ", "); got != "Yujin, Gaeul" {
			t.Errorf("idols = %q, want %q", got, "Yujin, Gaeul")
		}
		if len(groups) != 1 || groups[0] != "IVE" {
			t.Errorf("groups = %v, want [IVE]", groups)
		}
	})

	t.Run("a label whose group has since been renamed still resolves", func(t *testing.T) {
		// The group half disambiguates; it is not a requirement. Failing here
		// would turn a valid pick into "I don't know" over a rename.
		idols, _, unknown := resolveReuploadIdols(dir, "Yujin — Old Name,")
		if len(unknown) != 0 {
			t.Fatalf("unknown = %v, want none", unknown)
		}
		if len(idols) != 1 || idols[0] != "Yujin" {
			t.Errorf("idols = %v, want [Yujin]", idols)
		}
	})

	t.Run("a label for nobody is still unknown", func(t *testing.T) {
		_, _, unknown := resolveReuploadIdols(dir, "Nobody — Nowhere,")
		if len(unknown) != 1 || unknown[0] != "Nobody — Nowhere" {
			t.Errorf("unknown = %v, want [Nobody — Nowhere]", unknown)
		}
	})

	t.Run("the group half picks between idols sharing a name", func(t *testing.T) {
		shared := &directory{
			idols: []idolEntry{
				{id: "idl000000000010", name: "Yuna", groupID: "grp1", groupName: "ITZY"},
				{id: "idl000000000011", name: "Yuna", groupID: "grp2", groupName: "Brave Girls"},
			},
		}
		idol, ok := idolFromEntry(shared, "Yuna — Brave Girls")
		if !ok {
			t.Fatal("did not resolve")
		}
		if idol.id != "idl000000000011" {
			t.Errorf("id = %q, want the Brave Girls Yuna", idol.id)
		}
	})

	t.Run("unknown entries are reported, not silently dropped", func(t *testing.T) {
		_, _, unknown := resolveReuploadIdols(dir, "idl000000000001,Nobody")
		if len(unknown) != 1 || unknown[0] != "Nobody" {
			t.Errorf("unknown = %v, want [Nobody]", unknown)
		}
	})

	t.Run("the same idol twice collapses", func(t *testing.T) {
		idols, _, _ := resolveReuploadIdols(dir, "idl000000000001,Yujin")
		if len(idols) != 1 {
			t.Errorf("idols = %v, want one entry", idols)
		}
	})
}

func TestManualIngestRoles(t *testing.T) {
	const (
		newRole = "1530276704801656954"
		oldRole = "1530277196453773312"
	)

	t.Run("empty means the role check is off", func(t *testing.T) {
		t.Setenv("DISCORD_INGEST_ROLE_IDS", "")
		t.Setenv("DISCORD_REUPLOAD_ROLE_IDS", "")
		if got := manualIngestRoles(); len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})

	t.Run("reads the current name", func(t *testing.T) {
		t.Setenv("DISCORD_INGEST_ROLE_IDS", newRole)
		t.Setenv("DISCORD_REUPLOAD_ROLE_IDS", "")
		if got := manualIngestRoles(); len(got) != 1 || !got[snowflake.MustParse(newRole)] {
			t.Errorf("got %v, want just %s", got, newRole)
		}
	})

	// The compatibility path: this gate covered only /reupload until the context
	// menu joined it, so a deployment that still sets the old name must keep
	// working without an env change.
	t.Run("falls back to the legacy name", func(t *testing.T) {
		t.Setenv("DISCORD_INGEST_ROLE_IDS", "")
		t.Setenv("DISCORD_REUPLOAD_ROLE_IDS", oldRole)
		if got := manualIngestRoles(); len(got) != 1 || !got[snowflake.MustParse(oldRole)] {
			t.Errorf("got %v, want just %s", got, oldRole)
		}
	})

	t.Run("the current name wins over the legacy one", func(t *testing.T) {
		t.Setenv("DISCORD_INGEST_ROLE_IDS", newRole)
		t.Setenv("DISCORD_REUPLOAD_ROLE_IDS", oldRole)
		got := manualIngestRoles()
		if len(got) != 1 || !got[snowflake.MustParse(newRole)] {
			t.Errorf("got %v, want just %s", got, newRole)
		}
	})
}
