package bot

import (
	"fmt"
	"log/slog"

	disbot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// interactionResponder is the slice of a Discord interaction that reply needs.
//
// Slash commands, context menus and modal submits all satisfy it, which is what
// lets one two-audience helper serve all three — the context menu now answers
// with a modal, and the modal's submit is a SEPARATE interaction that has to
// report its result the same way.
type interactionResponder interface {
	DeferCreateMessage(ephemeral bool, opts ...rest.RequestOpt) error
	CreateMessage(messageCreate discord.MessageCreate, opts ...rest.RequestOpt) error
	Client() *disbot.Client
	ApplicationID() snowflake.ID
	Token() string
	Channel() discord.InteractionChannel
	User() discord.User
}

// reply mediates a slash command's two audiences: a successful result goes to
// the channel, while everything else — errors, empty lookups, bad input — is
// ephemeral and reaches only the person who ran the command.
//
// Discord fixes ephemerality when the interaction is FIRST acknowledged and it
// cannot be changed afterwards, so every command defers *ephemerally* and then
// publishes its result separately. Deferring publicly instead would leave a
// visible "thinking…" placeholder in the channel that turns into a public
// error message on failure.
//
// The result is posted as an ordinary channel message rather than an
// interaction follow-up. Follow-ups live under the interaction token, and
// deleting the ephemeral placeholder via that token took the follow-up with it
// — Discord resolved "the original response" to the newly posted message. A
// plain channel message is out of that token's reach entirely, so the two
// cannot be confused, and it leaves a normal permanent message behind.
type reply struct {
	e interactionResponder
	// What to call this in logs. A field rather than a method on the event,
	// because a modal submit has a custom id where a command has a name.
	label    string
	deferred bool
}

func newReply(e *events.ApplicationCommandInteractionCreate) *reply {
	return &reply{e: e, label: e.Data.CommandName()}
}

// newModalReply is the same for a modal submission, which arrives as its own
// interaction with its own token — the command's is already spent on showing
// the modal.
func newModalReply(e *events.ModalSubmitInteractionCreate) *reply {
	return &reply{e: e, label: e.Data.CustomID}
}

// Defer acknowledges the interaction with a private "thinking…" placeholder.
// Returns false when the acknowledgement itself failed, in which case the
// handler must stop — the interaction token is unusable.
func (r *reply) Defer() bool {
	if err := r.e.DeferCreateMessage(true); err != nil {
		slog.Error("could not defer interaction", "interaction", r.label, "err", err)
		return false
	}
	r.deferred = true
	return true
}

// Fail reports a problem to the invoking user only.
func (r *reply) Fail(format string, args ...any) {
	content := "❌ " + fmt.Sprintf(format, args...)

	if !r.deferred {
		if err := r.e.CreateMessage(discord.NewMessageCreate().WithEphemeral(true).WithContent(content)); err != nil {
			slog.Error("could not send failure reply", "interaction", r.label, "err", err)
		}
		return
	}
	if _, err := r.e.Client().Rest.UpdateInteractionResponse(r.e.ApplicationID(), r.e.Token(),
		discord.NewMessageUpdate().WithContent(content)); err != nil {
		slog.Error("could not edit failure reply", "interaction", r.label, "err", err)
	}
}

// Private reports a successful result to the invoking user only, leaving the
// channel untouched.
//
// For commands whose *effect* is elsewhere and whose result is just a receipt —
// /reupload puts content on the site and reacts to the source message, so a
// channel post would be noise in whatever social channel it was run in. The
// counterpart to Publish, which is for commands whose output IS the point.
func (r *reply) Private(format string, args ...any) {
	content := fmt.Sprintf(format, args...)

	if !r.deferred {
		if err := r.e.CreateMessage(discord.NewMessageCreate().WithEphemeral(true).WithContent(content)); err != nil {
			slog.Error("could not send private reply", "interaction", r.label, "err", err)
		}
		return
	}
	if _, err := r.e.Client().Rest.UpdateInteractionResponse(r.e.ApplicationID(), r.e.Token(),
		discord.NewMessageUpdate().WithContent(content)); err != nil {
		slog.Error("could not edit private reply", "interaction", r.label, "err", err)
	}
}

// Publish posts the result into the channel the command was run in and clears
// the private placeholder. Returns the posted message so callers that need a
// handle on it (pagination) can key state to it; nil when posting failed.
//
// Must be called after Defer — the placeholder is what gets cleared, and
// without it Discord reports the command as unanswered.
func (r *reply) Publish(create discord.MessageCreate) *discord.Message {
	msg, err := r.e.Client().Rest.CreateMessage(r.e.Channel().ID(), create)
	if err != nil {
		slog.Error("could not publish result", "interaction", r.label, "err", err)
		r.Fail("Could not post the result to this channel.")
		return nil
	}

	// The placeholder has served its purpose now the real message is up.
	// Losing this race only leaves a stale private "thinking…" behind, so a
	// failure here is logged rather than surfaced.
	if err := r.e.Client().Rest.DeleteInteractionResponse(r.e.ApplicationID(), r.e.Token()); err != nil {
		slog.Warn("could not clear ephemeral placeholder", "interaction", r.label, "err", err)
	}
	return msg
}

// respondModalEphemeral is respondEphemeral for a modal submission — the same
// "tell only the person who did this" answer, for the interaction type the
// context menu's modal comes back on.
func respondModalEphemeral(e *events.ModalSubmitInteractionCreate, content string) {
	if err := e.CreateMessage(discord.NewMessageCreate().WithEphemeral(true).WithContent(content)); err != nil {
		slog.Error("could not send ephemeral modal reply", "custom_id", e.Data.CustomID, "err", err)
	}
}
