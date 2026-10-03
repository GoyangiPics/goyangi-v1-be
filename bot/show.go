package bot

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
)

// /show posts a piece of content with buttons that hand each viewer whichever
// rendition they want.
//
// The gap it fills: pasting a goyangi link gets you exactly one rendering, and
// which one is right depends on the device — Safari below A17 Pro / M3 renders an
// AV1 MP4 as a blank frame, while the H.264 copy plays everywhere and the
// animated preview is the most portable of the three.
//
// # Why the buttons answer ephemerally
//
// Each click replies privately to the clicker, so two people can be looking at
// different renditions of the same post at once. The alternative — editing the
// posted message, which is what the /unwrap pagination does — would mean the last
// person to click changes it for everybody.
//
// # Why there is no state map
//
// The record id travels in the button's custom id, so these buttons keep working
// across a bot restart and never expire. The pagination states map exists because
// a page cursor has nowhere else to live; a format choice is stateless, so it
// doesn't need one.
const (
	showButtonPrefix = "gfmt"
	// discordCustomIDLimit is Discord's hard cap on a component custom id.
	discordCustomIDLimit = 100
)

func showCommandCreate() discord.SlashCommandCreate {
	return discord.SlashCommandCreate{
		Name:        "show",
		Description: "Post a piece of content with buttons to switch between HD, SD and preview.",
		Options: []discord.ApplicationCommandOption{
			discord.ApplicationCommandOptionString{
				Name:        "link",
				Description: "A goyangi link, an imgur mirror, or a cdn file URL",
				Required:    true,
			},
		},
	}
}

// showFormats are the buttons, in the order they appear.
var showFormats = []struct {
	key    string // short, because it shares the 100-char custom id budget
	format string
	label  string
	emoji  string
}{
	{"hd", formatMP4, "AV1 HD", "🎬"},
	{"sd", formatSD, "H.264 SD", "📱"},
	{"pv", formatPreview, "Preview", "🖼️"},
}

func showFormatByKey(key string) (string, string, bool) {
	for _, f := range showFormats {
		if f.key == key {
			return f.format, f.label, true
		}
	}
	return "", "", false
}

// showButtonRow builds the format buttons for a record.
//
// Returns ok=false when the record id is too long to fit a custom id. Ids are
// derived from group/idol names (see generateContentId), so a many-idol "mix"
// record can in principle run long — in that case the caller posts without
// buttons rather than sending Discord an over-length component.
func showButtonRow(recordID string) (discord.ActionRowComponent, bool) {
	buttons := make([]discord.InteractiveComponent, 0, len(showFormats))
	for _, f := range showFormats {
		customID := fmt.Sprintf("%s:%s:%s", showButtonPrefix, f.key, recordID)
		if len(customID) > discordCustomIDLimit {
			return discord.ActionRowComponent{}, false
		}
		buttons = append(buttons, discord.NewSecondaryButton(f.label, customID).WithEmoji(discord.ComponentEmoji{Name: f.emoji}))
	}
	return discord.NewActionRow(buttons...), true
}

func handleShowCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	r := newReply(e)
	if !r.Defer() {
		return
	}

	record, err := resolveContentRecord(data.String("link"))
	if err != nil {
		slog.Error("show: lookup failed", "err", err)
		r.Fail("Could not query the library.")
		return
	}
	if record == nil {
		r.Fail("No goyangi record matches that link. Try `/revive` to re-encode it instead.")
		return
	}

	// The content page, bare so Discord unfurls it into the rich embed — same
	// default as /random and /top's `embed` format, and it keeps the raw file URL
	// off the message.
	content := contentPageLink(record.Id)
	if title := strings.TrimSpace(record.GetString("title")); title != "" {
		content = fmt.Sprintf("-# %s\n%s", title, content)
	}

	create := discord.NewMessageCreate().WithContent(content)
	if row, ok := showButtonRow(record.Id); ok {
		create = discord.NewMessageCreate().WithContent(content).WithComponents(row)
	} else {
		slog.Warn("show: record id too long for format buttons", "record", record.Id)
	}

	r.Publish(create)
}

// handleShowFormatInteraction answers a format button, privately to the clicker.
func handleShowFormatInteraction(e *events.ComponentInteractionCreate, customID string) {
	// gfmt:<key>:<record id> — SplitN so a record id containing ':' (it can't
	// today, the id pattern is [a-z0-9-], but the parse shouldn't depend on that)
	// stays intact.
	parts := strings.SplitN(customID, ":", 3)
	if len(parts) != 3 {
		return
	}
	key, recordID := parts[1], parts[2]

	format, label, ok := showFormatByKey(key)
	if !ok {
		return
	}

	respond := func(text string) {
		if err := e.CreateMessage(discord.NewMessageCreate().
			WithEphemeral(true).
			WithContent(text),
		); err != nil {
			slog.Error("show: could not respond to format button", "err", err)
		}
	}

	record, err := App.FindRecordById("contents", recordID)
	if err != nil {
		// The record was deleted after the message was posted — the buttons
		// outlive it, since they carry no expiry.
		respond("That content no longer exists.")
		return
	}

	link := mediaLink(record, format)
	if link == "" {
		respond(fmt.Sprintf("No %s rendition available for this item.", label))
		return
	}

	respond(fmt.Sprintf("**%s**\n%s", label, link))
}
