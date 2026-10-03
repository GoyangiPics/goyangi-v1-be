package bot

import (
	"strings"
	"testing"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
)

// The custom id is the only thing carrying the target message from the
// right-click to the modal submission, and it is client-supplied — so it is
// both the load-bearing piece of the flow and untrusted input.
func TestParseIngestModalID(t *testing.T) {
	const (
		channel = "1372693175730438165"
		message = "1541355601421148311"
	)

	t.Run("round-trips what handleIngestContext builds", func(t *testing.T) {
		got, gotMsg, ok := parseIngestModalID(ingestModalPrefix + channel + ":" + message)
		if !ok {
			t.Fatal("did not parse")
		}
		if got != snowflake.MustParse(channel) {
			t.Errorf("channel = %s, want %s", got, channel)
		}
		if gotMsg != snowflake.MustParse(message) {
			t.Errorf("message = %s, want %s", gotMsg, message)
		}
	})

	t.Run("stays inside Discord's custom id limit", func(t *testing.T) {
		// 100 characters is the cap; two snowflakes and the prefix must fit with
		// room to spare, or the modal is rejected at send time.
		if got := len(ingestModalPrefix + channel + ":" + message); got > 100 {
			t.Errorf("custom id is %d chars, over the 100 limit", got)
		}
	})

	bad := []struct {
		name string
		id   string
	}{
		{"another modal's id", "somethingelse:1:2"},
		{"prefix only", ingestModalPrefix},
		{"one snowflake", ingestModalPrefix + channel},
		{"empty", ""},
		{"non-numeric channel", ingestModalPrefix + "abc:" + message},
		{"non-numeric message", ingestModalPrefix + channel + ":abc"},
		// Nothing here should ever panic or half-succeed on junk.
		{"separator only", ingestModalPrefix + ":"},
	}
	for _, tt := range bad {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			if _, _, ok := parseIngestModalID(tt.id); ok {
				t.Fatalf("parseIngestModalID(%q) should have failed", tt.id)
			}
		})
	}
}

// The pre-modal media check must stay off the network: it runs inside the
// 3-second window before the modal is the interaction's first response.
func TestHasIngestibleMedia(t *testing.T) {
	msg := func(content string, attachments ...discord.Attachment) discord.Message {
		return discord.Message{Content: content, Attachments: attachments}
	}

	t.Run("an attachment is enough", func(t *testing.T) {
		if !hasIngestibleMedia(msg("", discord.Attachment{URL: "https://cdn.discordapp.com/a.mp4"})) {
			t.Fatal("want true")
		}
	})

	t.Run("a direct media link counts", func(t *testing.T) {
		if !hasIngestibleMedia(msg("look https://files.catbox.moe/abc12.mp4")) {
			t.Fatal("want true")
		}
	})

	// The reason this helper exists: an album must register WITHOUT the HTTP
	// fetch resolving it would take. resolveImgurAlbum is a test-stubbable var,
	// so leaving it un-stubbed here doubles as proof nothing dials out — the
	// real implementation would fail loudly offline if it were reached.
	t.Run("an album counts without being resolved", func(t *testing.T) {
		if !hasIngestibleMedia(msg("https://imgur.com/a/kwon-eunbi-AbCd123")) {
			t.Fatal("want true")
		}
	})

	t.Run("plain text is not media", func(t *testing.T) {
		if hasIngestibleMedia(msg("no media here, just words")) {
			t.Fatal("want false")
		}
	})

	t.Run("empty message is not media", func(t *testing.T) {
		if hasIngestibleMedia(msg("")) {
			t.Fatal("want false")
		}
	})
}

func TestModalPrefill(t *testing.T) {
	t.Run("short values pass through", func(t *testing.T) {
		if got := modalPrefill("Yujin, Gaeul", 200); got != "Yujin, Gaeul" {
			t.Errorf("got %q", got)
		}
	})

	t.Run("clamps to the limit", func(t *testing.T) {
		long := strings.Repeat("a", 300)
		if got := modalPrefill(long, 200); len([]rune(got)) != 200 {
			t.Errorf("got %d runes, want 200", len([]rune(got)))
		}
	})

	t.Run("counts runes, not bytes", func(t *testing.T) {
		// A multi-byte name sliced by bytes would split a rune and produce
		// invalid UTF-8, which Discord also rejects.
		long := strings.Repeat("김", 250)
		got := modalPrefill(long, 200)
		if len([]rune(got)) != 200 {
			t.Errorf("got %d runes, want 200", len([]rune(got)))
		}
	})
}
