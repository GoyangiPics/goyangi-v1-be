package bot

import (
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

func TestUploaderFromMessage(t *testing.T) {
	relayApp := discord.User{Username: "WonheeBot", Bot: true}
	invoker := discord.User{Username: "nabi", GlobalName: ptr("Nabi 🐱")}

	tests := []struct {
		name       string
		msg        discord.Message
		wantName   string
		wantSource string
	}{
		{
			// The plain case, and the overwhelming majority: a person posted it
			// themselves, so the author IS the uploader.
			name:       "credits the author when there is no interaction",
			msg:        discord.Message{Author: discord.User{Username: "nabi"}},
			wantName:   "nabi",
			wantSource: "author",
		},
		{
			// The case this function was added for: a relay app's post that
			// happens to be the interaction response, so Discord tells us who ran
			// the command and the upload is credited to them instead of the app.
			name: "credits the invoker when the post is an interaction response",
			msg: discord.Message{
				Author:              relayApp,
				InteractionMetadata: &discord.InteractionMetadata{User: invoker},
			},
			wantName:   "nabi",
			wantSource: "interaction",
		},
		{
			name: "reads the deprecated interaction field too",
			msg: discord.Message{
				Author:      relayApp,
				Interaction: &discord.MessageInteraction{User: invoker},
			},
			wantName:   "nabi",
			wantSource: "interaction-legacy",
		},
		{
			name: "prefers the current field over the deprecated one",
			msg: discord.Message{
				Author:              relayApp,
				InteractionMetadata: &discord.InteractionMetadata{User: invoker},
				Interaction:         &discord.MessageInteraction{User: discord.User{Username: "stale"}},
			},
			wantName:   "nabi",
			wantSource: "interaction",
		},
		{
			// The expected outcome for a real anonymity feature: it posts a
			// separate message, so there is nothing to recover and the app keeps
			// the credit. Must not regress to an empty uploader.
			name:       "falls back to the app when a relayed post carries no invoker",
			msg:        discord.Message{Author: relayApp},
			wantName:   "WonheeBot",
			wantSource: "author",
		},
		{
			// A webhook post has no invoker field at all, by design.
			name: "falls back to the app for a webhook post",
			msg: discord.Message{
				Author:    discord.User{Username: "Relay Hook"},
				WebhookID: ptr(snowflake.ID(123)),
			},
			wantName:   "Relay Hook",
			wantSource: "author",
		},
		{
			// Defensive: an interaction whose user carries no username must not
			// blank the uploader, which is a required relation downstream.
			name: "ignores an interaction whose user has no username",
			msg: discord.Message{
				Author:              relayApp,
				InteractionMetadata: &discord.InteractionMetadata{User: discord.User{}},
			},
			wantName:   "WonheeBot",
			wantSource: "author",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotName, gotSource := uploaderFromMessage(tt.msg)
			if gotName != tt.wantName || gotSource != tt.wantSource {
				t.Errorf("uploaderFromMessage() = (%q, %q), want (%q, %q)",
					gotName, gotSource, tt.wantName, tt.wantSource)
			}
		})
	}
}

// A blocked uploader has to render as a deliberate skip, never as a failure:
// the reaction is the poster's only signal, and ❌ / no-reaction would read as
// "the bot broke" rather than "this was not ingested on purpose".
func TestIngestOutcomeSkipped(t *testing.T) {
	skipped := ingestOutcome{total: 4, skipped: "Not ingested — badposter is blocked from ingestion."}

	if got := skipped.emoji(); got != emojiSkipped {
		t.Errorf("emoji() = %q, want the skip marker %q", got, emojiSkipped)
	}
	if got := skipped.text(); got != "⏭️ Not ingested — badposter is blocked from ingestion." {
		t.Errorf("text() = %q, want the skip text", got)
	}

	// The skip has to win over the created==0 branch, which renders as a failure.
	if skipped.created != 0 {
		t.Fatal("fixture should have created nothing")
	}

	// And an ordinary zero-item run must still behave as it did before: no
	// reaction, failure text.
	empty := ingestOutcome{total: 1, failures: []string{"x.mp4 — nope"}}
	if got := empty.emoji(); got != "" {
		t.Errorf("emoji() on a failed run = %q, want no reaction", got)
	}
}

// A run where SOME items landed reacts with the goyangi emoji, not a warning.
//
// ⚠️ on a message reads in-channel as the bot correcting the person who posted
// it, when the thing that actually needs attention is a system_logs entry for
// whoever runs the bot. The reaction says "this was taken"; the log says what
// went wrong with it.
func TestIngestOutcomePartialSuccessStillSucceeds(t *testing.T) {
	partial := ingestOutcome{
		created:  2,
		total:    3,
		failures: []string{"c.mp4 — download failed"},
	}
	if got := partial.emoji(); got != emojiSuccess {
		t.Errorf("emoji() = %q, want the success marker %q", got, emojiSuccess)
	}

	// Unresolved names are the other partial case, and behave the same.
	unresolved := ingestOutcome{created: 1, total: 1, missing: []string{"idol Nobody"}}
	if got := unresolved.emoji(); got != emojiSuccess {
		t.Errorf("emoji() with unresolved names = %q, want the success marker %q", got, emojiSuccess)
	}

	// The text still reports what was missed — dropping the reaction must not
	// also drop the receipt the caller gets.
	if got := partial.text(); got == "" {
		t.Error("text() should still describe the partial result")
	}
}

// The GlobalName on the invoker above is deliberate: EffectiveName() would
// return it ("Nabi 🐱") while these expect the Username ("nabi"). Somebody
// posting directly and through a relay has to resolve to one uploader record, so
// both paths key on Username — see the note on uploaderFromMessage.
func TestUploaderFromMessageIgnoresGlobalName(t *testing.T) {
	msg := discord.Message{
		Author: discord.User{Username: "WonheeBot", Bot: true},
		InteractionMetadata: &discord.InteractionMetadata{
			User: discord.User{Username: "nabi", GlobalName: ptr("Nabi 🐱")},
		},
	}
	if name, _ := uploaderFromMessage(msg); name != "nabi" {
		t.Errorf("uploaderFromMessage() = %q, want the username %q", name, "nabi")
	}
}

// The credit line a relay app leaves when its user opts out of anonymity. Only
// the parsing is covered here; the match against existing uploaders needs the
// database and is deliberately match-only (see creditFromPostedBy).
func TestPostedByName(t *testing.T) {
	embedWith := func(em discord.Embed) discord.Message {
		return discord.Message{Author: discord.User{Username: "WonheeBot", Bot: true}, Embeds: []discord.Embed{em}}
	}
	tests := []struct {
		name string
		msg  discord.Message
		want string
	}{
		{
			name: "plain line among metadata lines",
			msg:  discord.Message{Content: "idol: Wonhee\ngroup: ILLIT\nPosted by: Trailsofjamie"},
			want: "Trailsofjamie",
		},
		{
			name: "case-insensitive label",
			msg:  discord.Message{Content: "posted BY: someone"},
			want: "someone",
		},
		{
			// An app is free to render the label bold, or the name.
			name: "bold label and bold name",
			msg:  discord.Message{Content: "**Posted by:** **nabi**"},
			want: "nabi",
		},
		{
			name: "mention-style name",
			msg:  discord.Message{Content: "Posted by: @nabi"},
			want: "nabi",
		},
		{
			name: "name with spaces survives intact",
			msg:  discord.Message{Content: "Posted by: Chae Won Fan  \nsome caption"},
			want: "Chae Won Fan",
		},
		{
			name: "embed description",
			msg:  embedWith(discord.Embed{Description: "New post!\nPosted by: nabi"}),
			want: "nabi",
		},
		{
			name: "embed field named Posted by",
			msg:  embedWith(discord.Embed{Fields: []discord.EmbedField{{Name: "Posted by", Value: "nabi"}}}),
			want: "nabi",
		},
		{
			name: "embed footer",
			msg:  embedWith(discord.Embed{Footer: &discord.EmbedFooter{Text: "Posted by: nabi"}}),
			want: "nabi",
		},
		{
			// The anonymous case, and the one that must not match: the app never
			// wrote the line, so there is nothing to credit.
			name: "no line",
			msg:  discord.Message{Content: "idol: Wonhee\ngroup: ILLIT"},
			want: "",
		},
		{
			// "posted by" inside a sentence is prose, not a credit line.
			name: "label must start the line",
			msg:  discord.Message{Content: "this was posted by: someone yesterday"},
			want: "",
		},
		{
			name: "empty name after the colon",
			msg:  discord.Message{Content: "Posted by: ** **"},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := postedByName(tt.msg); got != tt.want {
				t.Errorf("postedByName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A credited display name is rarely the plain handle the uploader record is
// named after. "kuro🍊" was the first real miss.
func TestPostedByCandidates(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"plain handle is tried once", "kuro", []string{"kuro"}},
		{"trailing emoji", "kuro🍊", []string{"kuro🍊", "kuro"}},
		{"emoji both sides and inside", "✨ku🍊ro✨", []string{"✨ku🍊ro✨", "kuro"}},
		{"username punctuation survives", "not.kuro_44", []string{"not.kuro_44"}},
		{"accents survive", "Chaé", []string{"Chaé"}},
		{"symbols only leaves a single candidate", "🍊🍊", []string{"🍊🍊"}},
		{"decoration around spaces", "「kuro fan」", []string{"「kuro fan」", "kuro fan"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := postedByCandidates(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("postedByCandidates(%q) = %q, want %q", tt.in, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("postedByCandidates(%q)[%d] = %q, want %q", tt.in, i, got[i], tt.want[i])
				}
			}
		})
	}
}
