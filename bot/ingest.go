package bot

import (
	"encoding/base64"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"goyangi-v1-be/hooks"

	disbot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/snowflake/v2"
)

// Media ingestion has four entry points, split by whether they fire on their
// own or somebody asked for them.
//
// The PASSIVE triggers (1–3) are restricted to the channels in
// DISCORD_ALLOWED_CHANNEL_IDS (see config.go): they watch messages nobody
// pointed at the bot, so the allowlist is what decides where it may watch.
//
//  1. @-mention of the bot: metadata comes from `key: value` lines.
//  2. Role ping: idol/group come from the pinged "Idol [Group]" role names;
//     `key: value` lines can add/override.
//  3. Reply to a message that created a set: the new items join that set,
//     reusing its metadata.
//
// The EXPLICIT trigger works in any channel, because there is nothing to
// decide — a person named one message and asked for it. It is gated on role
// instead, the same way /reupload is (manualIngestRoleIDs).
//
//  4. The "Ingest this message" context-menu command
//     (commands.go dispatches it to handleIngestContext below).
//
// Every media item (attachment or recognized link) becomes a "contents"
// record with the file attached; the R2 hook then transcodes and moves it to
// public storage, exactly like an upload from the web UI.

// inboundMessage is one Discord message offered to the passive triggers (1–3),
// from whichever door it came through: this process's own gateway connection
// (onMessageCreate) or another bot forwarding it over the ingest API
// (ingest_api.go). Everything the triggers need beyond the message itself is
// here, so the decisions below run identically either way.
type inboundMessage struct {
	msg     discord.Message
	guildID snowflake.ID
	// door is the bot that delivered the message: ours on the gateway, the
	// forwarding bot over the API.
	door snowflake.ID
	// relay says whose post this is, when the forwarding bot posted it on
	// someone's behalf. Only the ingest API sets it.
	relay *relayCredit
	// roleNames resolves pinged role ids to role names ("Idol [Group]"),
	// leaving out any it can't name.
	roleNames func(ids []snowflake.ID) ([]string, error)
	// dryRun decides everything and changes nothing: no claim, no records, no
	// chain bookkeeping, no system_logs entries.
	dryRun bool

	// poster is who the message is credited to. Worked out by ingestInbound
	// before the triggers run, because follow-ups and replies match on it.
	poster poster
}

// relayCredit is the forwarding bot's word on whose post one of its own
// messages is: a person, or nobody it may name.
type relayCredit struct {
	anonymous bool
	userID    string
	username  string
}

// jumpLink is the message's discord.com link, which is also what records store
// in `contents.discord` and what log entries carry as their context handle.
func (in inboundMessage) jumpLink() string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", in.guildID, in.msg.ChannelID, in.msg.ID)
}

// collectMedia is the message's media. A dry run lists imgur albums as
// themselves instead of looking them up: it has to answer quickly, and a lookup
// is a network call that may fail and log.
func (in inboundMessage) collectMedia() []MediaItem {
	if in.dryRun {
		return collectMediaUnresolved(in.msg)
	}
	return collectMedia(in.msg)
}

// postedAt is when the message was posted, read from its id: Discord's clock,
// not ours.
func (in inboundMessage) postedAt() time.Time {
	return in.msg.ID.Time()
}

// ingestStatus is what happened to an inbound message, in the words the ingest
// API reports it.
type ingestStatus string

const (
	// Not for us: outside the allowlist, no trigger, or no media.
	statusIgnored ingestStatus = "ignored"
	// Already handled: a redelivery, a retry, or the other door got there first.
	statusDuplicate ingestStatus = "duplicate"
	// Deliberately not ingested. Left unmarked, like a failure — see ingestOutcome.emoji.
	statusSkipped ingestStatus = "skipped"
	// At least one item became a post.
	statusIngested ingestStatus = "ingested"
	// Attempted, and nothing was saved. The detail is in system_logs.
	statusFailed ingestStatus = "failed"
	// A manual request from someone who may not make it. Nothing was done.
	statusDenied ingestStatus = "denied"
)

// Why, as stable codes. The ingest API hands them to another developer's bot,
// which may branch on them, so they never change meaning and never carry
// internal error text — that stays in system_logs.
const (
	reasonSystemMessage    = "system_message"
	reasonOwnMessage       = "own_message"
	reasonChannelNotListed = "channel_not_listed"
	reasonNoMedia          = "no_media"
	reasonNoTrigger        = "no_trigger"
	reasonAlreadyHandled   = "already_handled"
	reasonAlreadyArchived  = "already_archived"
	reasonReannouncement   = "reannouncement"
	reasonBlocked          = "blocked"
	reasonOptedOut         = "opted_out"
	reasonUnresolved       = "unresolved_names"
	reasonNothingSaved     = "nothing_saved"
	reasonInternal         = "internal_error"

	// Manual requests ("Ingest this message" from another bot) only.
	reasonServerNotListed = "server_not_listed" // the message's server isn't in INGEST_GUILD_IDS
	reasonNoPermission    = "no_permission"     // lacks every DISCORD_INGEST_ROLE_IDS role
	reasonCannotUpload    = "cannot_upload"     // not linked to a goyangi account allowed to upload
	reasonBadSet          = "bad_set"           // the set link is wrong or the set doesn't exist
	reasonUnknownIdols    = "unknown_idols"     // names goyangi doesn't know
	reasonNoIdols         = "no_idols"          // none given
)

// ingestResult is the verdict on one inbound message.
type ingestResult struct {
	status  ingestStatus
	reason  string // a reason code, when there is one
	outcome ingestOutcome
	setID   string         // the set the items went into, when any were created
	preview *ingestPreview // what would happen; dry runs only
}

// reaction is the marker for the triggering message, or "" for none. See
// ingestOutcome.emoji for why only ingestion gets one.
func (r ingestResult) reaction() string {
	if r.status == statusIngested {
		return emojiSuccess
	}
	return ""
}

// onMessageCreate feeds the passive triggers (1–3) from our own gateway.
//
// It takes the message's place in its channel's line right here, in the
// gateway's order, and does the work in a goroutine. disgo runs listeners one
// at a time, slash commands included, so waiting here — behind a forwarded
// message's long download in the same channel, say — would stall every event
// the bot gets.
func onMessageCreate(e *events.MessageCreate) {
	defer recoverHandler("messageCreate")

	// Ingestion is guild-only (records link back to a guild message URL).
	if e.GuildID == nil {
		return
	}
	turn := queueTurn(e.ChannelID)
	client, guildID := e.Client(), *e.GuildID
	in := inboundMessage{
		msg:     e.Message,
		guildID: guildID,
		door:    client.ApplicationID,
		roleNames: func(ids []snowflake.ID) ([]string, error) {
			return getRoleNamesFromIDs(client, guildID, ids)
		},
	}

	// Counted in Busy() until done, like the handler it continues.
	handlersInFlight.Add(1)
	go func() {
		defer handlersInFlight.Add(-1)
		defer turn.release()
		defer recoverHandler("messageCreate")
		turn.wait()
		reactTo(client, e.ChannelID, e.MessageID, ingestInbound(in).reaction())
	}()
}

// ─── Which bots stand for goyangi ────────────────────────────────────────────
//
// Two bots can stand for goyangi in a channel: ours, and a bot forwarding
// messages over the ingest API. People @-mention either one to ask for
// ingestion, "role ping + @-mention" means "already on the site" whichever one
// they name, and neither one's own posts are content. So both doors recognise
// both bots. With each door knowing only its own, a re-announcement that one
// door skipped was ingested by the other.

// ourBotID is this process's own bot. Set at Init from DISCORD_TOKEN — a bot
// token's first segment is the bot's user id — so a mention of it still counts
// while it is disabled but still in a server, and on Start from the client.
var ourBotID atomic.Uint64

var (
	forwardersMu sync.Mutex
	// forwarders are the bots that have forwarded messages over the ingest API.
	forwarders = map[snowflake.ID]bool{}
)

func rememberForwarder(id snowflake.ID) {
	forwardersMu.Lock()
	defer forwardersMu.Unlock()
	forwarders[id] = true
}

// isGoyangiBot reports whether id is one of the bots that stand for goyangi.
func (in inboundMessage) isGoyangiBot(id snowflake.ID) bool {
	if id == 0 {
		return false
	}
	if id == in.door || id == snowflake.ID(ourBotID.Load()) {
		return true
	}
	forwardersMu.Lock()
	defer forwardersMu.Unlock()
	return forwarders[id]
}

// userMentionRegexp matches a user mention as it appears in message text.
var userMentionRegexp = regexp.MustCompile(`<@!?(\d+)>`)

// mentionsGoyangiBot reports whether the text @-mentions one of goyangi's bots.
// Deliberately content-based: a reply that pings a bot's message fills the
// mentions array but not the content, and must NOT count as a mention.
func (in inboundMessage) mentionsGoyangiBot() bool {
	for _, m := range userMentionRegexp.FindAllStringSubmatch(in.msg.Content, -1) {
		if id, err := snowflake.Parse(m[1]); err == nil && in.isGoyangiBot(id) {
			return true
		}
	}
	return false
}

// botIDFromToken reads the bot's user id out of a bot token, whose first
// segment is that id in base64. Zero when the token isn't shaped like one.
func botIDFromToken(token string) snowflake.ID {
	first, _, _ := strings.Cut(strings.TrimSpace(token), ".")
	for _, enc := range []*base64.Encoding{base64.RawStdEncoding, base64.StdEncoding, base64.RawURLEncoding, base64.URLEncoding} {
		if raw, err := enc.DecodeString(first); err == nil {
			if id, err := snowflake.Parse(string(raw)); err == nil {
				return id
			}
		}
	}
	return 0
}

// ─── Who a message is credited to ────────────────────────────────────────────

// poster is who a message is credited to, and the key that ties it to the same
// person's other messages: follow-ups, replies and repeat pings.
type poster struct {
	name   string // the uploader to credit, unless an `uploader:` line names one
	source string // which signal decided it, for the log
	// key is the person's Discord user id when it is known, the credited name
	// when only a relay's "Posted by:" line says who it was, and EMPTY when the
	// post can't be tied to a person — a relay app's post that names nobody, or
	// one its poster asked to keep anonymous. An empty key never opens or joins
	// a set across messages: two anonymous posts through the same app are not
	// one person's drop, and filing them together would put one person's post
	// under somebody else's name.
	key     string
	relayed bool // posted by an app or webhook on someone's behalf
	// app is the posting app's id when key is empty because Discord names only
	// the app. It never chains bare messages together, but a reply from the
	// same app to one of its own sets continues that set: replying is the
	// app's own word that the two belong together — dedicated per-idol bots
	// post a drop exactly that way — and the set is credited to the app
	// either way, so nobody's post lands under someone else's name. Empty for
	// a post the forwarding bot calls anonymous: it said there's nobody.
	app string
}

// canReplyTo reports whether a reply by p may join set: the same person, or,
// for a post tied to nobody, the same app replying to its own keyless set.
func (p poster) canReplyTo(set Metadata) bool {
	if p.key != "" {
		return set.AuthorID == p.key
	}
	return p.app != "" && set.AuthorID == "" && set.AppID == p.app
}

// creditFor decides who a message is credited to. See creditFromMessage for the
// signals Discord gives and creditFromPostedBy for a relay's own credit line.
func creditFor(in inboundMessage) poster {
	msg := in.msg
	if r := in.relay; r != nil {
		if r.anonymous {
			return poster{name: msg.Author.Username, source: "relay-anonymous", relayed: true}
		}
		return poster{name: r.username, source: "relay", key: r.userID, relayed: true}
	}

	name, source, userID := creditFromMessage(msg)
	p := poster{name: name, source: source, key: userID, relayed: msg.Author.Bot || msg.WebhookID != nil}
	if p.relayed && source == "author" {
		// Discord names only the app. Its own "Posted by:" line is the next best
		// thing; without one the post stays the app's, tied to nobody.
		p.key = ""
		if credited, ok := creditFromPostedBy(msg); ok {
			p.name, p.source, p.key = credited, "posted-by", "posted-by:"+strings.ToLower(credited)
		} else {
			p.app = msg.Author.ID.String()
		}
	}
	return p
}

// ingestInbound runs the passive triggers (1–3) on one message, wherever it
// came from, and reports what happened. The caller owns any feedback in
// Discord, and holds the channel's turn (queueTurn) unless this is a dry run.
func ingestInbound(in inboundMessage) ingestResult {
	msg := in.msg

	// Other bots and webhook apps are legitimate posters — people routinely use
	// them to post content into the scraping channels. Only Discord's own system
	// messages are not, and goyangi's own bots: our AVIF-ready notices carry cdn
	// links and would be re-ingested in a loop. The exception is a post the
	// forwarding bot made on someone's behalf and says so (`relay`).
	if msg.Author.System {
		return ingestResult{status: statusIgnored, reason: reasonSystemMessage}
	}
	if in.relay == nil && in.isGoyangiBot(msg.Author.ID) {
		return ingestResult{status: statusIgnored, reason: reasonOwnMessage}
	}
	// Confined to the allowed channels, whatever the trigger: goyangi must not
	// re-upload media from anywhere else.
	if !allowedChannelIDs[msg.ChannelID] {
		return ingestResult{status: statusIgnored, reason: reasonChannelNotListed}
	}

	in.poster = creditFor(in)
	plan := prepareIngestion(in)
	switch {
	case plan.skipped != "":
		return ingestResult{status: statusSkipped, reason: plan.skipped}
	case !plan.ok && plan.ignored == reasonNoTrigger && alreadyIngested(in.jumpLink()):
		// A follow-up or reply resent after a restart, or after the hour: what
		// tied it to its set is gone, but its posts aren't. Say so, rather
		// than calling an archived message unarchivable.
		return ingestResult{status: statusDuplicate, reason: reasonAlreadyArchived}
	case !plan.ok:
		return ingestResult{status: statusIgnored, reason: plan.ignored}
	}

	// An explicit `uploader:` line wins; a reply or follow-up keeps the credit
	// of the set it joins, which is the same person by construction.
	if plan.metadata.Uploader == "" {
		plan.metadata.Uploader = in.poster.name
	}
	stampMessage(&plan.metadata, msg, in.guildID)
	plan.metadata.AuthorID = in.poster.key
	plan.metadata.AppID = in.poster.app

	if in.dryRun {
		return dryRunVerdict(in, plan)
	}

	// Skip redeliveries of an already-handled message. Claim before any side
	// effect (records, chain bookkeeping) so a redelivery can't double them.
	if !claimMessage(msg.ID.String()) {
		slog.Info("skipping duplicate message event", "id", msg.ID)
		return ingestResult{status: statusDuplicate, reason: reasonAlreadyHandled}
	}
	// The claim is in memory; the archive itself covers a retry after a
	// restart, and a message the other door took before a switch-over.
	if alreadyIngested(in.jumpLink()) {
		slog.Info("ingest: already in the archive", "discord", in.jumpLink())
		return ingestResult{status: statusDuplicate, reason: reasonAlreadyArchived}
	}

	// A panic past this point would leave the message claimed for an hour,
	// though nothing was saved — and a retry would be turned away as already
	// handled. Give it back, then let the panic carry on to the handler's guard.
	defer func() {
		if r := recover(); r != nil {
			releaseClaim(msg.ID.String())
			panic(r)
		}
	}()

	channelID := msg.ChannelID.String()
	if plan.viaChain {
		// No author id in the line: stdout lands in the host journal with
		// unbounded retention, and the set's own discord link identifies the
		// message if one ever needs reviewing.
		slog.Info("ingest: joining the poster's open chain",
			"channel", channelID, "set", plan.metadata.SetId)
	}
	// Logged only for relayed posts, which are the ones that pile into one
	// bucket — and this line is how we find out whether a given relay app
	// leaves the person recoverable at all, from real traffic rather than by
	// reading its source. stdout, not system_logs: nothing has gone wrong.
	if in.poster.relayed {
		slog.Info("ingest: crediting a relayed post",
			"uploader", plan.metadata.Uploader,
			"source", in.poster.source,
			"app", msg.Author.Username,
			"webhook", msg.WebhookID != nil,
		)
	}

	outcome := runIngestion(plan.metadata, plan.items, plan.joinsSet, true)
	logOutcome(outcome, in.jumpLink())

	// The follow-up slot is spent only by a follow-up that landed: one whose
	// items all failed hasn't joined anything, and its retry mustn't pay twice.
	// Spent after the claim, too, so a redelivery can't burn one.
	if plan.viaChain && outcome.created > 0 {
		consumeChain(channelID, in.poster.key, in.postedAt())
	}

	result := ingestResult{status: statusFailed, reason: reasonNothingSaved, outcome: outcome}
	switch {
	case outcome.skipped != "":
		result.status, result.reason = statusSkipped, outcome.skipCode
	case outcome.fatal != "":
		result.reason = outcome.fatalCode
	case outcome.created > 0:
		result.status, result.reason = statusIngested, ""
	}

	switch {
	case outcome.created == 0:
		// Nothing was saved, so nothing makes a second attempt a duplicate: a
		// retry through the ingest API gets a real one.
		releaseClaim(msg.ID.String())
	case plan.joinsSet:
		result.setID = plan.metadata.SetId
	default:
		// A fresh set opens a chain for its poster, so the bare messages that
		// follow join it. runIngestion took metadata by value and stamped the
		// new set id on its own copy, so the registry it wrote is where that
		// copy can be read back.
		if setMeta, found := lookupSet(msg.ID.String()); found {
			result.setID = setMeta.SetId
			if in.poster.key != "" {
				openChain(channelID, in.poster.key, setMeta, in.postedAt())
			}
		}
	}
	return result
}

// ingestModalPrefix marks a modal submission as belonging to the context menu,
// and carries the message it was invoked on.
//
// A context-menu command cannot declare options — Discord only allows those on
// slash commands — so the idol list has to be asked for afterwards, and a modal
// is the only place to ask. The modal's submit arrives as a fresh interaction
// with no memory of what was right-clicked, so the target rides along in the
// custom id: "ingest:<channelID>:<messageID>". Two snowflakes and a prefix is
// ~46 characters, well inside Discord's 100-character limit.
const ingestModalPrefix = "ingest:"

// handleIngestContext handles the "Ingest this message" context-menu command:
// the same pipeline as /reupload, reached by right-clicking the message instead
// of pasting a link to it.
//
// Works in ANY channel. The channel allowlist exists for the passive triggers,
// which fire on messages nobody pointed at the bot — it decides where the bot
// may watch. Nothing here fires on its own, so it carries /reupload's gates
// instead: the role check, and the caller's account being allowed to upload.
//
// This only gets as far as SHOWING the modal. Everything after the person fills
// it in is handleIngestModal below.
func handleIngestContext(e *events.ApplicationCommandInteractionCreate, data discord.MessageCommandInteractionData) {
	if e.GuildID() == nil {
		respondEphemeral(e, "Ingestion only works in a server.")
		return
	}
	if !callerMayIngestAnywhere(e.Member()) {
		respondEphemeral(e, "You don't have permission to use this command.")
		return
	}
	// The name is re-resolved on the modal submit — that is the interaction that
	// ingests; this check exists to refuse BEFORE the person types anything.
	if denial, _ := callerUploadDenial(e.User().EffectiveName(), e.User().Username); denial != "" {
		respondEphemeral(e, string(denial))
		return
	}

	msg := data.TargetMessage()
	// Other bots' posts are ingestible (see onMessageCreate); only our own
	// output is not.
	if msg.Author.ID == e.Client().ApplicationID {
		respondEphemeral(e, "The bot's own messages can't be ingested.")
		return
	}
	// Checked BEFORE the modal, from the resolved message the interaction
	// already carries — asking someone to name idols and only then telling them
	// there was nothing to ingest wastes the one thing this flow costs them.
	//
	// hasIngestibleMedia, NOT collectMedia: this runs inside the 3-second window
	// Discord allows before the modal is the first response, and collectMedia
	// resolves imgur albums with an HTTP fetch each — two slow album pages and
	// the modal never opens. The cheap check counts an album without resolving
	// it; the real resolution happens after the submit, behind a Defer.
	if !hasIngestibleMedia(msg) {
		respondEphemeral(e, "No ingestible media found in that message (attachments or supported links).")
		return
	}

	// Best-effort prefill from any `key: value` lines already in the message, so
	// a post that DID state its idols doesn't have to be retyped. Errors are
	// ignored: this is a convenience, and the modal is about to ask anyway.
	var seed Metadata
	_ = extractMetadata(msg.Content, &seed)

	// Prefills are clamped to the input's own MaxLength: Discord rejects the
	// whole ModalCreate when a Value exceeds it, so an over-long `idol:` line in
	// the target message would otherwise kill the command as "This interaction
	// failed" — precisely on the messages rich enough to prefill.
	idols := discord.NewShortTextInput("idols").
		WithRequired(true).
		WithMaxLength(200).
		WithPlaceholder("Yujin, Gaeul").
		WithValue(modalPrefill(seed.Idol, 200))
	tags := discord.NewShortTextInput("tags").
		WithRequired(false).
		WithMaxLength(200).
		WithPlaceholder("fancam, 4k").
		WithValue(modalPrefill(seed.Tags, 200))
	set := discord.NewShortTextInput("set").
		WithRequired(false).
		WithMaxLength(200).
		WithPlaceholder("https://goyangi.pics/set/…")

	if err := e.Modal(discord.ModalCreate{
		CustomID: ingestModalPrefix + msg.ChannelID.String() + ":" + msg.ID.String(),
		Title:    "Ingest this message",
		Components: []discord.LayoutComponent{
			// No autocomplete in a modal — Discord has none for text inputs — so
			// these are typed. resolveReuploadIdols has always accepted
			// hand-typed names and reports the ones it can't place, which is
			// what makes that workable.
			discord.NewLabel("Idols (comma-separated)", idols),
			discord.NewLabel("Tags (optional)", tags),
			discord.NewLabel("Add to an existing set (optional)", set),
		},
	}); err != nil {
		slog.Error("could not open the ingest modal", "message", msg.ID, "err", err)
	}
}

// handleIngestModal runs the ingestion once the context menu's modal comes back.
//
// Every gate is re-checked here. The custom id is client-supplied and this is a
// separate interaction from the one that passed the checks, so nothing about
// that first pass can be trusted to still hold — or to have happened at all.
func handleIngestModal(e *events.ModalSubmitInteractionCreate) {
	if e.GuildID() == nil {
		respondModalEphemeral(e, "Ingestion only works in a server.")
		return
	}
	if !callerMayIngestAnywhere(e.Member()) {
		respondModalEphemeral(e, "You don't have permission to use this command.")
		return
	}
	denial, uploaderName := callerUploadDenial(e.User().EffectiveName(), e.User().Username)
	if denial != "" {
		respondModalEphemeral(e, string(denial))
		return
	}

	channelID, messageID, ok := parseIngestModalID(e.Data.CustomID)
	if !ok {
		respondModalEphemeral(e, "I lost track of which message this was for — try again.")
		return
	}

	dir, err := loadDirectory()
	if err != nil {
		respondModalEphemeral(e, "Could not load the idol/group directory. Try again shortly.")
		return
	}
	idolNames, groupNames, unknown := resolveReuploadIdols(dir, e.Data.Text("idols"))
	if len(unknown) > 0 {
		respondModalEphemeral(e, fmt.Sprintf("I don't know: %s.", strings.Join(unknown, ", ")))
		return
	}
	if len(idolNames) == 0 {
		respondModalEphemeral(e, "Name at least one idol.")
		return
	}

	runManualIngest(newModalReply(e), manualIngest{
		channelID:  channelID,
		messageID:  messageID,
		guildID:    *e.GuildID(),
		idolNames:  idolNames,
		groupNames: groupNames,
		tagsRaw:    e.Data.Text("tags"),
		setRaw:     e.Data.Text("set"),
		// The caller, not the message author — the same credit /reupload gives.
		// The gate's canonical name, for the reason callerUploadDenial gives.
		uploader: uploaderName,
	})
}

// hasIngestibleMedia reports whether collectMedia could find anything, without
// the network round-trips its album resolution costs.
//
// Albums are counted as media WITHOUT being resolved — resolution is an HTTP
// fetch of the album page, deferred to the post-submit pipeline. The cost of
// the shortcut is one false positive: an album that later resolves to nothing
// gets the modal asked and then "no ingestible media" after the submit, which
// beats the command dying unanswered on a slow album page.
func hasIngestibleMedia(m discord.Message) bool {
	if len(m.Attachments) > 0 {
		return true
	}
	content := stripMetadataLines(m.Content)
	if imgurAlbumRegexp.MatchString(content) {
		return true
	}
	// With album links stripped, extractMediaLinks' album pass has nothing to
	// resolve, and every remaining pass is pure string work.
	return len(extractMediaLinks(imgurAlbumRegexp.ReplaceAllString(content, ""))) > 0
}

// modalPrefill bounds a seed value to a text input's MaxLength.
func modalPrefill(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// parseIngestModalID reads the target back out of a modal custom id.
func parseIngestModalID(customID string) (channelID, messageID snowflake.ID, ok bool) {
	rest, found := strings.CutPrefix(customID, ingestModalPrefix)
	if !found {
		return 0, 0, false
	}
	channelRaw, messageRaw, found := strings.Cut(rest, ":")
	if !found {
		return 0, 0, false
	}
	channel, cerr := snowflake.Parse(channelRaw)
	message, merr := snowflake.Parse(messageRaw)
	if cerr != nil || merr != nil {
		return 0, 0, false
	}
	return channel, message, true
}

// ingestPlan is what prepareIngestion decided to do with a message.
//
// `joinsSet` covers both ways of adding to an existing set — an explicit reply
// and a bare chain follow-up — because runIngestion treats them identically: no
// new set, reuse the remembered metadata. `viaChain` is tracked separately
// only so the chain's follow-up budget is spent at the right moment.
type ingestPlan struct {
	metadata Metadata
	items    []MediaItem
	joinsSet bool
	viaChain bool
	ok       bool
	// trigger names the rule that fired: role_ping, repeat_ping, mention,
	// reply, follow_up or text. Reported by dry runs.
	trigger string
	// ignored is the reason code when !ok.
	ignored string
	// skipped is set (to a reason code) when the message is deliberately NOT
	// ingested, as distinct from merely ignored.
	skipped string
}

// prepareIngestion decides whether the message should be ingested and, if so,
// assembles its metadata and media items. The allowlist and the poster are
// already settled (ingestInbound).
func prepareIngestion(in inboundMessage) ingestPlan {
	msg := in.msg
	mentioned := in.mentionsGoyangiBot()

	// A message carrying BOTH a role ping and an @-mention of goyangi's bot is
	// someone re-announcing content that is already in the library: the pings
	// are for people, and ingesting again would duplicate the records. Checked
	// before everything else, media included — the answer is a skip either way.
	//
	// An @-mention alone still ingests (with `key: value` metadata) and a role
	// ping alone still ingests passively — only the combination is a skip. The
	// cost is that "@bot + metadata lines + an incidental role ping so people see
	// it" used to work and now won't, silently.
	//
	// Returns before claimMessage, so a redelivery reports the skip again —
	// nothing is done for it, so no dedupe is needed.
	if mentioned && len(msg.MentionRoles) > 0 {
		return ingestPlan{skipped: reasonReannouncement}
	}

	items := in.collectMedia()
	if len(items) == 0 {
		return ingestPlan{ignored: reasonNoMedia}
	}
	channelID, key, at := msg.ChannelID.String(), in.poster.key, in.postedAt()

	switch {
	// Explicit @-mention.
	case mentioned:
		metadata := Metadata{}
		if err := extractMetadata(msg.Content, &metadata); err == nil {
			return ingestPlan{metadata: metadata, items: items, ok: true, trigger: "mention"}
		}
		// No `idol:` and `group:` lines. The message gets every chance it
		// would have had without the mention — a reply, a follow-up, the text —
		// because a bot that does other things too gets mentioned for those as
		// well, and a mention mustn't make a message worse off than none.
		slog.Info("@-mention without idol/group lines, carrying on without it", "discord", in.jumpLink())

	// Role ping: passive ingestion.
	case len(msg.MentionRoles) > 0:
		pingRoleNames, err := in.roleNames(msg.MentionRoles)
		if err != nil {
			slog.Error("unable to resolve role names", "err", err)
			return ingestPlan{ignored: reasonNoTrigger}
		}

		metadata := rolesToMetadata(pingRoleNames)
		if err := extractMetadata(msg.Content, &metadata); err != nil {
			// Roles were pinged but none parse as "Idol [Group]" — a group-only
			// role like @ifeye, or a role that isn't about an idol at all.
			//
			// Hand it to text detection rather than giving up, with the role
			// names added to what gets scanned: "@ifeye Rahee" then resolves,
			// the group coming from the role and the idol from the text. Before
			// this, a group ping made the message WORSE off than no ping at all,
			// because it claimed the message and then bailed.
			slog.Info("role ping without usable idol/group, trying the text", "roles", pingRoleNames)
			return textDetection(in, items, pingRoleNames)
		}

		// A reply that pings the same roles again, to a set its poster may reply
		// to (poster.canReplyTo), continues that set like a plain reply would:
		// the ping is for people, the reply says where it belongs. A different
		// subject still opens its own set, as a repeat ping's does.
		if replied := repliedTo(msg); replied != 0 {
			if remembered, found := lookupSet(replied.String()); found && in.poster.canReplyTo(remembered) && sameSubject(remembered, metadata) {
				return ingestPlan{metadata: remembered, items: items, joinsSet: true, ok: true, trigger: "reply"}
			}
		}

		// Re-pinging the same roles is how plenty of people post one drop: three
		// messages in a row, each with its own ping, rather than a reply chain.
		// Taken literally that is three sets of the same thing, so a repeat ping
		// about the same subject joins the set the first one opened. It spends a
		// follow-up slot like any other continuation, so the same 3-message
		// budget applies.
		if key != "" {
			if open, ok := peekChain(channelID, key, at); ok && sameSubject(open, metadata) {
				return ingestPlan{metadata: open, items: items, joinsSet: true, viaChain: true, ok: true, trigger: "repeat_ping"}
			}
		}
		return ingestPlan{metadata: metadata, items: items, ok: true, trigger: "role_ping"}
	}

	// Reply, by the same person, to a message that created a set: append to it.
	// A relay app's post that names nobody counts as the app's own here (see
	// poster.app). A reply to something else entirely can still be a plain
	// follow-up in a drop — people reply to their own earlier message just for
	// threading — so a miss here falls through to the chain check.
	if replied := repliedTo(msg); replied != 0 {
		if remembered, found := lookupSet(replied.String()); found && in.poster.canReplyTo(remembered) {
			return ingestPlan{metadata: remembered, items: items, joinsSet: true, ok: true, trigger: "reply"}
		}
	}

	// A bare follow-up: no trigger of its own (anything with one was handled
	// above), posted by the same person, in the same channel, shortly after a
	// message that opened a set. The loosest rule in the file and the only one
	// that infers intent — see state.go for the trade it makes.
	if key != "" {
		if meta, ok := peekChain(channelID, key, at); ok {
			return ingestPlan{metadata: meta, items: items, joinsSet: true, viaChain: true, ok: true, trigger: "follow_up"}
		}
	}

	return textDetection(in, items, nil)
}

// repliedTo is the id of the message this one replies to, or 0. The gateway
// sends the whole `referenced_message`; `message_reference` alone is enough,
// for a reply whose original was deleted or a message object built by hand. A
// forward (Discord's "Forward") also carries a reference, and is not a reply.
func repliedTo(msg discord.Message) snowflake.ID {
	if msg.ReferencedMessage != nil {
		return msg.ReferencedMessage.ID
	}
	if ref := msg.MessageReference; ref != nil && ref.Type == discord.MessageReferenceTypeDefault && ref.MessageID != nil {
		return *ref.MessageID
	}
	return 0
}

// sameSubject reports whether two ingestions are about the same idols and the
// same groups. Order and case don't matter — the same roles pinged in a
// different order are the same drop.
//
// Deliberately exact rather than overlapping: a second ping that adds an idol is
// a different subject and gets its own set, because merging two sets afterwards
// is easy and splitting a wrongly-merged one is not.
func sameSubject(a, b Metadata) bool {
	return equalNameSets(a.Idol, b.Idol) && equalNameSets(a.Group, b.Group)
}

func equalNameSets(a, b string) bool {
	left, right := splitTrim(a), splitTrim(b)
	if len(left) != len(right) || len(left) == 0 {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, name := range left {
		counts[strings.ToLower(name)]++
	}
	for _, name := range right {
		key := strings.ToLower(name)
		if counts[key] == 0 {
			return false
		}
		counts[key]--
	}
	return true
}

// textDetection is the last resort: no usable trigger, but the message names an
// idol the directory knows. See detect.go for why this exists and how hard it
// tries not to guess.
//
// `extraTerms` is scanned alongside the message text. Pinged role names go in
// there, so a group-only ping still contributes its group.
func textDetection(in inboundMessage, items []MediaItem, extraTerms []string) ingestPlan {
	none := ingestPlan{ignored: reasonNoTrigger}

	dir, err := loadDirectory()
	if err != nil {
		return none
	}
	content := in.msg.Content
	if len(extraTerms) > 0 {
		content += "\n" + strings.Join(extraTerms, "\n")
	}
	found, ok := detectSubjects(dir, content)
	if !ok {
		return none
	}

	// Logged for every hit, at warning level, on purpose: this is the only rule
	// that decides an attribution nobody stated, and the log is how its hit rate
	// and its mistakes get reviewed. Noise here is the point — but not for a dry
	// run, which ingests nothing.
	if !in.dryRun {
		hooks.LogBotWarning("ingested from message text (no ping)", map[string]any{
			"discord": in.jumpLink(),
			"idols":   strings.Join(found.idolNames, ", "),
			"groups":  strings.Join(found.groupNames, ", "),
			"matched": strings.Join(found.matched, ", "),
		})
	}

	metadata := Metadata{
		Idol:  strings.Join(found.idolNames, ", "),
		Group: strings.Join(found.groupNames, ", "),
	}
	// `key: value` lines still win — someone who wrote them meant them.
	if err := extractMetadata(in.msg.Content, &metadata); err != nil {
		return none
	}
	return ingestPlan{metadata: metadata, items: items, ok: true, trigger: "text"}
}

// uploaderFromMessage decides who an ingested message is credited to, and
// reports which signal decided it. creditFromMessage, without the user id.
func uploaderFromMessage(msg discord.Message) (name string, source string) {
	name, source, _ = creditFromMessage(msg)
	return name, source
}

// creditFromMessage decides who an ingested message is credited to, which
// signal decided it, and that person's Discord user id.
//
// # Why the interaction check exists
//
// Relay apps — "post this anonymously for me" bots — post on somebody's behalf,
// so `author` is the app and every upload through it lands in a single uploader
// bucket under the app's name. Discord exposes the human in exactly one case: a
// message that IS an interaction response carries `interaction_metadata.user`,
// the person who ran the command.
//
// That case is narrower than it sounds. An app that posts a *separate* message
// after the command — which is what an anonymity feature wants, since otherwise
// the metadata gives the poster away — leaves nothing to recover, and a webhook
// message has no invoker field at all. So this is opportunistic: when the signal
// is there we use it, and when it isn't the app's own name remains the uploader
// exactly as before. Nothing regresses either way.
//
// `Username` rather than EffectiveName(), matching the author path below it: a
// person posting directly and through a relay has to resolve to ONE name, or the
// two doors mint two uploader records for them — the problem hooks/uploaders.go
// exists to clean up. (reupload.go uses EffectiveName for its own attribution,
// which is a pre-existing divergence rather than a precedent.)
func creditFromMessage(msg discord.Message) (name, source, userID string) {
	if md := msg.InteractionMetadata; md != nil && md.User.Username != "" {
		return md.User.Username, "interaction", md.User.ID.String()
	}
	// The deprecated predecessor of the field above. Still populated on older
	// messages, and cheap to keep reading.
	if in := msg.Interaction; in != nil && in.User.Username != "" {
		return in.User.Username, "interaction-legacy", in.User.ID.String()
	}
	return msg.Author.Username, "author", msg.Author.ID.String()
}

// postedByRegexp matches the credit line a relay app leaves when its user opts
// out of anonymity:
//
//	Posted by: Trailsofjamie
//
// Case-insensitive, and tolerant of Discord bold/italics around the label or
// the name, since an app is free to render it `**Posted by:** name`. The name
// runs to the end of the line; cleanPostedByName trims the decoration off it.
var postedByRegexp = regexp.MustCompile(`(?im)^\s*[*_]*posted by[*_]*\s*:\s*(.+?)\s*$`)

// postedByName returns the name a relay app credited the post to, or "" when
// the message carries no such line.
//
// Looks in the message text first, then in each embed's description, fields
// and footer — an app can put its credit line in any of them, and a field named
// "Posted by" with the name as its value is the same statement in a different
// shape, so it is joined back into one line before matching.
func postedByName(msg discord.Message) string {
	texts := []string{msg.Content}
	for _, em := range msg.Embeds {
		texts = append(texts, em.Description)
		for _, f := range em.Fields {
			texts = append(texts, f.Name+": "+f.Value)
		}
		if em.Footer != nil {
			texts = append(texts, em.Footer.Text)
		}
	}
	for _, text := range texts {
		if m := postedByRegexp.FindStringSubmatch(text); m != nil {
			if name := cleanPostedByName(m[1]); name != "" {
				return name
			}
		}
	}
	return ""
}

// cleanPostedByName strips the markdown and mention decoration a credit line
// may wrap the name in: bold or italic markers, backticks, or a leading @.
func cleanPostedByName(raw string) string {
	name := strings.Trim(raw, "*_` \t")
	name = strings.TrimPrefix(name, "@")
	return strings.TrimSpace(name)
}

// postedByCandidates is the list of names to try, in order, for a credited
// name: the name as written, then the name with decoration removed.
//
// The first real miss was "kuro🍊" against an uploader called "kuro": display
// names carry emoji, symbols and the like, and a person's uploader record is
// named after the plain handle. So the second candidate keeps letters, digits,
// marks (accents), spaces and the handful of punctuation a Discord username can
// contain, and drops everything else. Dedupe, so an undecorated name is looked
// up once.
func postedByCandidates(name string) []string {
	candidates := []string{name}
	stripped := strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsMark(r) || unicode.IsSpace(r) {
			return r
		}
		if strings.ContainsRune("_.-'", r) {
			return r
		}
		return -1
	}, name))
	if stripped != "" && !strings.EqualFold(stripped, name) {
		candidates = append(candidates, stripped)
	}
	return candidates
}

// creditFromPostedBy resolves a relayed post's "Posted by:" line to an EXISTING
// uploader and returns that record's canonical name.
//
// Match-only, by name or alias, and deliberately so: the line is text the relay
// app rendered from whatever its user is called there, which need not be the
// name they upload under here. Minting an uploader from it would create the
// duplicate-profile problem hooks/uploaders.go exists to repair, one post at a
// time. A name that matches nobody is logged and the post stays credited to the
// app, exactly as before — add the name as an alias on their profile and the
// next post lands right.
//
// The canonical `name` is returned rather than the line's text so that
// resolveRelations' lookupOrCreateByName lands on the same record.
func creditFromPostedBy(msg discord.Message) (name string, ok bool) {
	posted := postedByName(msg)
	if posted == "" {
		return "", false
	}
	// Candidates are tried in order, so the exact name still wins when both a
	// decorated and an undecorated profile exist.
	rec, err := findUploaderByNames(postedByCandidates(posted)...)
	if err != nil {
		slog.Warn("ingest: could not look up posted-by name", "name", posted, "err", err)
		return "", false
	}
	if rec == nil {
		slog.Info("ingest: posted-by name matches no uploader; crediting the app",
			"name", posted, "app", msg.Author.Username)
		return "", false
	}
	return rec.GetString("name"), true
}

// stampMessage stamps the message-level fields every entry point shares.
func stampMessage(metadata *Metadata, msg discord.Message, guildID snowflake.ID) {
	metadata.Discord = fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, msg.ChannelID, msg.ID)
	metadata.MessageID = msg.ID.String()
}

// fillMessageDefaults is the explicit paths' version (reupload.go): credit the
// message author — or the person behind a relay app, see creditFor — unless the
// metadata already names an uploader, and stamp the message. The passive
// triggers do the same in ingestInbound, with the poster they worked out first.
func fillMessageDefaults(metadata *Metadata, msg discord.Message, guildID snowflake.ID) {
	if metadata.Uploader == "" {
		p := creditFor(inboundMessage{msg: msg})
		metadata.Uploader = p.name
		if p.relayed {
			slog.Info("ingest: crediting a relayed post",
				"uploader", p.name,
				"source", p.source,
				"app", msg.Author.Username,
				"webhook", msg.WebhookID != nil,
			)
		}
	}
	stampMessage(metadata, msg, guildID)
	metadata.AuthorID = msg.Author.ID.String()
}

// ─── Pipeline ────────────────────────────────────────────────────────────────

// ingestOutcome is what an ingestion run reports back to the poster.
type ingestOutcome struct {
	created  int
	total    int
	failures []string // "<filename> — <error>" per failed item, for people
	// failedItems are the failed items' filenames alone — what the ingest API
	// may tell another developer's bot. The errors stay in system_logs.
	failedItems []string
	missing     []string // idol/group names that didn't resolve
	fatal       string   // set when nothing was attempted at all
	fatalCode   string   // the reason code for fatal
	// Set when the run was deliberately abandoned rather than attempted — a
	// blocked uploader. Distinct from `fatal`: nothing went wrong, so this must
	// not read as an error anywhere.
	skipped  string
	skipCode string // the reason code for skipped
}

// emoji is the reaction marker for this outcome. Configurable — see config.go.
// Note this is the *reaction* form; ingestOutcome.text() keeps plain unicode
// because that string is message content, where custom emoji need the
// `<:name:id>` tag form instead.
//
// Only ingestion is marked, so that no mark is ever a complaint:
//
//   - Anything ingested → the goyangi emoji, whether or not every item made it.
//     A partial run used to react ⚠️, which reads in-channel as the bot telling
//     somebody their post was wrong, when what actually happened is that some
//     items landed and the rest are a problem for whoever runs the bot.
//     logOutcome already records those to system_logs as a warning, which is
//     where they can be acted on.
//   - A total failure → NOTHING, for the same reason ❌ was never used: stamping
//     one on someone's post is a public telling-off, and the detail is in
//     system_logs either way.
//   - A deliberate skip → NOTHING too. It used to get ⏭️ so a blocked poster
//     wouldn't keep re-posting into a void, but on somebody's post it read as
//     a brush-off. The skip is still in the logs (stdout) and in the ephemeral
//     reply to a manual request.
func (o ingestOutcome) emoji() string {
	switch {
	case o.skipped != "" || o.fatal != "" || o.created == 0:
		return ""
	default:
		return emojiSuccess
	}
}

// text renders the outcome as one human-readable message.
func (o ingestOutcome) text() string {
	switch {
	case o.skipped != "":
		return "⏭️ " + o.skipped
	case o.fatal != "":
		return "❌ " + o.fatal
	case o.created == 0:
		return "❌ No items could be ingested:\n" + bulletList(o.failures)
	case len(o.failures) > 0:
		return fmt.Sprintf("⚠️ Ingested %d/%d items. Failed:\n%s", o.created, o.total, bulletList(o.failures))
	default:
		out := fmt.Sprintf("✅ Ingested %d item(s).", o.created)
		if len(o.missing) > 0 {
			out += " Skipped unresolved names: " + strings.Join(o.missing, ", ")
		}
		return out
	}
}

// runIngestion resolves relations, creates the set (for multi-item messages)
// and one content record per media item. Pure pipeline — no Discord I/O —
// so every entry point shares it.
// automatic is true for the passive path — a role ping, a reply, a chain or text
// detection in an allowlisted channel — and false for the explicit "Ingest this
// message" and /reupload, which somebody asked for on purpose.
func runIngestion(metadata Metadata, items []MediaItem, isReply, automatic bool) ingestOutcome {
	outcome := ingestOutcome{total: len(items)}

	rel, err := resolveRelations(metadata)
	if err != nil {
		slog.Error("relation resolution failed", "err", err)
		outcome.fatal = fmt.Sprintf("could not resolve names: %v", err)
		outcome.fatalCode = reasonInternal
		return outcome
	}
	outcome.missing = rel.missing

	// Blocked uploader: abandon before anything is downloaded or created.
	//
	// Checked here, at the top of the shared pipeline, so it covers every entry
	// point — a passive role ping, "Ingest this message", and /reupload alike.
	// And checked at INGEST rather than in the encode pipeline: skipping the
	// encode instead would leave a real content record with no renditions, which
	// is the permanently-broken state queue_api.go describes, visible on the site
	// as an item that never finishes.
	//
	// Deliberately a quality filter, not a security control: an explicit
	// `uploader:` line naming somebody else, or posting through a relay app,
	// still gets in. Blocking those means blocking on Discord author id, which is
	// what config-level allowlists are for.
	if len(rel.blockedUploaders) > 0 {
		outcome.skipped = fmt.Sprintf("Not ingested — %s is blocked from ingestion.",
			strings.Join(rel.blockedUploaders, ", "))
		outcome.skipCode = reasonBlocked
		return outcome
	}

	// The uploader's own opt-out, for people who upload on the site and also
	// post the same thing to Discord. Automatic runs only: a deliberate
	// "Ingest this message" or /reupload is someone asking for exactly this.
	// Any credited uploader opting out skips the message — it is their post
	// either way, and a message credits one uploader in practice.
	if automatic && len(rel.optedOutUploaders) > 0 {
		outcome.skipped = fmt.Sprintf("Not imported — %s turned off automatic import from Discord.",
			strings.Join(rel.optedOutUploaders, ", "))
		outcome.skipCode = reasonOptedOut
		return outcome
	}

	// idol/group are required on both contents and sets — fail up front with
	// the actual unresolved names instead of per-item validation errors.
	if len(rel.idolIDs) == 0 || len(rel.groupIDs) == 0 {
		outcome.fatal = "no matching idol/group found in the database"
		if len(rel.missing) > 0 {
			outcome.fatal += " (unresolved: " + strings.Join(rel.missing, ", ") + ")"
		}
		outcome.fatalCode = reasonUnresolved
		return outcome
	}

	// What was stated and what resolved can differ: "Eunbi [IZONE]" files under
	// Kwon Eunbi / Solo, because the idol wins over a group that doesn't contain
	// her (see resolveRelations). Left alone, the record's relations would say
	// Solo while its title said IZONE — and the title is what the site and the
	// social embeds show.
	//
	// Only a generated title is rewritten. An explicit `title:` line is the
	// poster's own words; comparing against autoTitle is how the two are told
	// apart.
	if metadata.Title == autoTitle(metadata.Idol, metadata.Group) {
		resolvedIdols := strings.Join(namesByIDs("groups_idols", rel.idolIDs), ", ")
		resolvedGroups := strings.Join(namesByIDs("groups", rel.groupIDs), ", ")
		if retitled := autoTitle(resolvedIdols, resolvedGroups); retitled != "" && retitled != metadata.Title {
			slog.Info("retitled from the resolved names", "from", metadata.Title, "to", retitled)
			metadata.Title = retitled
		}
	}

	// A set is created for every non-reply ingestion, single item included.
	// The site's main listing reads "contents_sets" (see useFetchItems.ts
	// allSets), so a setless content record is invisible there — a single
	// pinged item used to vanish from the front page. Mirrors the web upload
	// path, which also always creates a set. Stickers are the one exception,
	// matching uploads.vue.
	if !isReply && metadata.Filetype != "sticker" {
		setId, err := createSetRecord(metadata, rel)
		if err != nil {
			slog.Error("set record creation failed", "err", err)
			outcome.fatal = fmt.Sprintf("could not create set: %v", err)
			outcome.fatalCode = reasonInternal
			return outcome
		}
		metadata.SetId = setId
		rememberSet(metadata.MessageID, metadata)
	}

	for _, item := range items {
		if _, err := createContentRecord(item, metadata, rel); err != nil {
			slog.Warn("unable to process media item", "url", item.URL, "err", err)
			outcome.failures = append(outcome.failures, fmt.Sprintf("%s — %v", item.Filename, err))
			outcome.failedItems = append(outcome.failedItems, item.Filename)
		} else {
			outcome.created++
		}
	}

	// The set is created before the downloads, so a run where every item failed
	// leaves it empty. An empty set is a broken card on the front page and a
	// reply target for nothing — remove it, and let a retry start clean.
	if !isReply && metadata.SetId != "" && outcome.created == 0 {
		forgetSet(metadata.MessageID)
		if set, err := App.FindRecordById("contents_sets", metadata.SetId); err == nil {
			if err := App.Delete(set); err != nil {
				slog.Warn("could not remove an empty set", "set", metadata.SetId, "err", err)
			}
		}
	}

	// AVIF-ready notifications fire separately from the R2 hook via
	// hooks.OnAvifReady once transcoding finishes; this is just the intake
	// receipt.
	slog.Info("ingestion finished", "message", metadata.MessageID, "created", outcome.created, "failed", len(outcome.failures))
	return outcome
}

// getRoleNamesFromIDs resolves role IDs to their names within the guild.
func getRoleNamesFromIDs(client *disbot.Client, guildID snowflake.ID, roleIDs []snowflake.ID) ([]string, error) {
	guildRoles, err := client.Rest.GetRoles(guildID)
	if err != nil {
		return nil, err
	}

	guildRoleMap := make(map[snowflake.ID]string, len(guildRoles))
	for _, role := range guildRoles {
		guildRoleMap[role.ID] = role.Name
	}

	roleNames := make([]string, 0, len(roleIDs))
	for _, roleID := range roleIDs {
		if name, ok := guildRoleMap[roleID]; ok {
			roleNames = append(roleNames, name)
		}
	}

	return roleNames, nil
}

// ─── Poster feedback ─────────────────────────────────────────────────────────
// Only a reaction is left in the channel — the detail goes to the "system_logs"
// collection (logOutcome) so the scraping channels stay free of bot chatter.

// logOutcome records anything that went wrong during an ingestion run. A clean
// run writes nothing — system_logs is for problems, not an audit trail.
func logOutcome(o ingestOutcome, msgURL string) {
	ctx := map[string]any{
		"discord":  msgURL,
		"created":  o.created,
		"total":    o.total,
		"failures": o.failures,
		"missing":  o.missing,
	}

	switch {
	// Expected behaviour, so stdout only — system_logs is for problems, not an
	// audit trail (see the note at the top of this function).
	case o.skipped != "":
		slog.Info("ingest: skipped", "reason", o.skipped, "discord", msgURL)
	case o.fatal != "":
		hooks.LogBotError("ingestion failed: "+o.fatal, ctx)
	case o.created == 0:
		hooks.LogBotError("ingestion failed: no items could be ingested", ctx)
	case len(o.failures) > 0:
		hooks.LogBotWarning(
			fmt.Sprintf("ingestion partially failed: %d/%d items created", o.created, o.total), ctx)
	case len(o.missing) > 0:
		hooks.LogBotWarning("ingested with unresolved names: "+strings.Join(o.missing, ", "), ctx)
	}
}

func reactTo(client *disbot.Client, channelID, messageID snowflake.ID, emoji string) {
	// An empty emoji means "say nothing" — see ingestOutcome.emoji, where
	// failure deliberately produces no reaction.
	if emoji == "" {
		return
	}
	if err := client.Rest.AddReaction(channelID, messageID, emoji); err != nil {
		// The reaction is the only in-channel signal left, and a wrong or
		// inaccessible custom emoji id fails exactly here — surface it rather
		// than letting ingestion look like it silently did nothing.
		hooks.LogBotWarning("could not add reaction: "+err.Error(), map[string]any{
			"emoji":   emoji,
			"channel": channelID.String(),
			"message": messageID.String(),
		})
	}
}

// NOTE: the bot no longer posts replies into the scraping channels. Feedback
// there is a reaction only; the detail lives in the "system_logs" collection
// (see logOutcome) and in the ephemeral response of the context-menu command.

func bulletList(lines []string) string {
	return "- " + strings.Join(lines, "\n- ")
}
