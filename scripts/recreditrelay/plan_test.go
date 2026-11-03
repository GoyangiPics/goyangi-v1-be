package main

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

func TestParseJumpLink(t *testing.T) {
	want := messageRef{guild: 124767749099618304, channel: 124767749099618305, message: 1558653170094313485}
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"what the bot writes", "https://discord.com/channels/124767749099618304/124767749099618305/1558653170094313485", true},
		{"older host", "https://discordapp.com/channels/124767749099618304/124767749099618305/1558653170094313485", true},
		{"client host", "https://ptb.discord.com/channels/124767749099618304/124767749099618305/1558653170094313485", true},
		{"trailing slash and space", " https://discord.com/channels/124767749099618304/124767749099618305/1558653170094313485/ ", true},
		{"empty", "", false},
		{"a channel, not a message", "https://discord.com/channels/124767749099618304/124767749099618305", false},
		{"a DM", "https://discord.com/channels/@me/124767749099618305/1558653170094313485", false},
		{"not discord", "https://example.com/channels/124767749099618304/124767749099618305/1558653170094313485", false},
		{"not an id", "https://discord.com/channels/124767749099618304/general/1558653170094313485", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseJumpLink(tt.in)
			if ok != tt.ok {
				t.Fatalf("parseJumpLink(%q) ok = %v, want %v", tt.in, ok, tt.ok)
			}
			if ok && got != want {
				t.Errorf("parseJumpLink(%q) = %+v, want %+v", tt.in, got, want)
			}
		})
	}
}

func TestByMessageFetchesEachMessageOnce(t *testing.T) {
	const link = "https://discord.com/channels/1/2/3"
	msgs, noLink := byMessage([]post{
		{ID: "a", Discord: link},
		{ID: "b", Discord: "https://discord.com/channels/1/2/4"},
		// The same message spelt another way is still one fetch.
		{ID: "c", Discord: "https://discordapp.com/channels/1/2/3"},
		{ID: "d", Discord: ""},
	})
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].link != link || len(msgs[0].posts) != 2 || msgs[0].posts[1].ID != "c" {
		t.Errorf("first message = %+v, want posts a and c under %s", msgs[0], link)
	}
	if len(noLink) != 1 || noLink[0].ID != "d" {
		t.Errorf("noLink = %+v, want post d", noLink)
	}
}

const relayID = "relayapp000000a"

func appMessage(content string) discord.Message {
	return discord.Message{Author: discord.User{ID: 999, Username: "Kyujin Bot", Bot: true}, Content: content}
}

func TestDecide(t *testing.T) {
	dir := newDirectory([]uploader{
		{ID: relayID, Name: "Kyujin Bot"},
		{ID: "kuro", Name: "kuro"},
		{ID: "nabi", Name: "Nabi", Aliases: "nabi.dev, 나비"},
		{ID: "blocked", Name: "spammer", BlockIngest: true},
		{ID: "optout", Name: "siteonly", SkipDiscordImport: true},
	})
	webhook := snowflake.ID(77)
	tests := []struct {
		name    string
		msg     discord.Message
		outcome outcome
		target  string
		posted  string
	}{
		{
			name:    "small-text credit with a decorated display name",
			msg:     appMessage("idol: Karina\n-# Posted by: kuro🍊"),
			outcome: moves, target: "kuro", posted: "kuro🍊",
		},
		{
			name:    "an alias counts",
			msg:     appMessage("-# Posted by: 나비"),
			outcome: moves, target: "nabi", posted: "나비",
		},
		{
			name: "a credit in an embed's footer",
			msg: discord.Message{
				Author: discord.User{Username: "Kyujin Bot", Bot: true},
				Embeds: []discord.Embed{{Footer: &discord.EmbedFooter{Text: "Posted by: NABI.DEV"}}},
			},
			outcome: moves, target: "nabi", posted: "NABI.DEV",
		},
		{
			name: "a webhook is a relay too",
			msg: discord.Message{
				Author: discord.User{Username: "Kyujin Bot"}, WebhookID: &webhook,
				Content: "-# Posted by: kuro",
			},
			outcome: moves, target: "kuro", posted: "kuro",
		},
		{
			name:    "no line is an anonymous post",
			msg:     appMessage("idol: Karina\ngroup: aespa"),
			outcome: anonymous,
		},
		{
			name:    "a name nobody has stays with the app",
			msg:     appMessage("-# Posted by: stranger🍊"),
			outcome: unmatched, posted: "stranger🍊",
		},
		{
			name:    "blocked",
			msg:     appMessage("-# Posted by: spammer"),
			outcome: optedOut, target: "blocked", posted: "spammer",
		},
		{
			name:    "opted out of Discord import",
			msg:     appMessage("-# Posted by: siteonly"),
			outcome: optedOut, target: "optout", posted: "siteonly",
		},
		{
			name:    "the line names the app itself",
			msg:     appMessage("-# Posted by: Kyujin Bot"),
			outcome: namesRelay, target: relayID, posted: "Kyujin Bot",
		},
		{
			name: "a person's own message is not the app's to re-credit",
			msg: discord.Message{
				Author:  discord.User{Username: "kuro"},
				Content: "-# Posted by: nabi",
			},
			outcome: notRelayed,
		},
		{
			name: "Discord naming who ran the command wins over the line, so it is left alone",
			msg: discord.Message{
				Author:              discord.User{Username: "Kyujin Bot", Bot: true},
				InteractionMetadata: &discord.InteractionMetadata{User: discord.User{Username: "kuro"}},
				Content:             "-# Posted by: nabi",
			},
			outcome: viaInteraction,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := dir.decide(tt.msg, relayID)
			if v.outcome != tt.outcome {
				t.Fatalf("outcome = %v, want %v", v.outcome, tt.outcome)
			}
			if v.posted != tt.posted {
				t.Errorf("posted = %q, want %q", v.posted, tt.posted)
			}
			var target string
			if v.target != nil {
				target = v.target.ID
			}
			if target != tt.target {
				t.Errorf("target = %q, want %q", target, tt.target)
			}
		})
	}
}

// Two records answering to one name resolve to the newer, as the live bot's
// findUploaderByNames does — the directory is passed newest first.
func TestDecidePicksTheNewestOfTwoNamesakes(t *testing.T) {
	dir := newDirectory([]uploader{
		{ID: "newer", Name: "kuro"},
		{ID: "older", Name: "Kuro"},
	})
	if v := dir.decide(appMessage("-# Posted by: kuro"), relayID); v.target == nil || v.target.ID != "newer" {
		t.Errorf("target = %+v, want newer", v.target)
	}
}

func TestPlanSets(t *testing.T) {
	kuro, nabi := &uploader{ID: "kuro"}, &uploader{ID: "nabi"}
	move := func(id, set string, to *uploader) result {
		return result{post: post{ID: id, Set: set}, verdict: verdict{outcome: moves, target: to}}
	}
	stay := func(id, set string) result {
		return result{post: post{ID: id, Set: set}, verdict: verdict{outcome: anonymous}}
	}
	results := []result{
		// all of the app's posts move: the app leaves
		move("a1", "A", kuro), move("a2", "A", kuro),
		// one stays: the app stays beside kuro
		move("b1", "B", kuro), stay("b2", "B"),
		// two people: shared, and the app leaves
		move("c1", "C", kuro), move("c2", "C", nabi),
		// nothing moves: not touched
		stay("d1", "D"),
		// no set at all
		move("e1", "", kuro),
	}
	p := planSets(results, func(r result) bool { return r.outcome == moves })
	if p.touched != 3 || p.appLeaves != 2 || p.appStays != 1 || p.shared != 1 {
		t.Errorf("plan = %+v, want 3 touched, 2 leaves, 1 stays, 1 shared", p)
	}
	for set, want := range map[string]bool{"A": false, "B": true, "C": false} {
		if got, ok := p.stays[set]; !ok || got != want {
			t.Errorf("stays[%s] = %v (present %v), want %v", set, got, ok, want)
		}
	}
	if _, ok := p.stays["D"]; ok {
		t.Error("set D had no moves and should not be in the plan")
	}

	// After a commit, a failed write is a post that stayed: the app stays on A.
	failed := planSets(results, func(r result) bool { return r.outcome == moves && r.post.ID != "a2" })
	if !failed.stays["A"] {
		t.Error("with a2's write failed, the app should still be on set A")
	}
}

func TestTallies(t *testing.T) {
	kuro := &uploader{ID: "kuro", Name: "kuro"}
	results := []result{
		{post: post{ID: "1", Discord: "m1"}, verdict: verdict{outcome: moves, target: kuro}},
		{post: post{ID: "2", Discord: "m1"}, verdict: verdict{outcome: moves, target: kuro}},
		{post: post{ID: "3", Discord: "m2"}, verdict: verdict{outcome: moves, target: kuro}},
		{post: post{ID: "4", Discord: "m3"}, verdict: verdict{outcome: unmatched, posted: "x"}},
		{post: post{ID: "5", Discord: "m4"}, verdict: verdict{outcome: unmatched, posted: "y"}},
		{post: post{ID: "6", Discord: "m4"}, verdict: verdict{outcome: unmatched, posted: "y"}},
	}
	counts := countOutcomes(results)
	if c := counts[moves]; c.posts != 3 || c.messages != 2 {
		t.Errorf("moves = %+v, want 3 posts from 2 messages", c)
	}
	if c := counts[unmatched]; c.posts != 3 || c.messages != 2 {
		t.Errorf("unmatched = %+v, want 3 posts from 2 messages", c)
	}

	names := tallyBy(results, func(r result) (string, string, bool) { return r.posted, r.posted, r.outcome == unmatched })
	if len(names) != 2 || names[0].name != "y" || names[0].posts != 2 || names[0].messages != 1 || names[1].name != "x" {
		t.Errorf("unmatched names = %+v, want y (2 posts, 1 message) then x", names)
	}
}
