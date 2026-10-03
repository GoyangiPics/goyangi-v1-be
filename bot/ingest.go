package bot

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"
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

// onMessageCreate handles the passive triggers (1–3).
func onMessageCreate(e *events.MessageCreate) {
	defer recoverHandler("messageCreate")

	// Other bots and webhook apps are legitimate posters — people routinely use
	// them to post content into the scraping channels. Only skip ourselves
	// (our own AVIF-ready notices carry cdn links and would be re-ingested in a
	// loop) and Discord's own system messages (joins, pins, boosts).
	if e.Message.Author.ID == e.Client().ApplicationID || e.Message.Author.System {
		return
	}
	// Ingestion is guild-only (records link back to a guild message URL).
	if e.GuildID == nil {
		return
	}

	plan := prepareIngestion(e)
	if !plan.ok {
		return
	}

	// Skip Discord redeliveries of an already-handled message. Claim before
	// any side effect (replies, records) so a redelivery can't double them.
	if !claimMessage(e.MessageID.String()) {
		slog.Info("skipping duplicate message event", "id", e.MessageID)
		return
	}

	channelID, authorID := e.ChannelID.String(), e.Message.Author.ID.String()

	// Spent here rather than in peekChain, so a redelivery that never gets past
	// the claim above can't burn a follow-up slot.
	if plan.viaChain {
		consumeChain(channelID, authorID)
		// No author id in the line: stdout lands in the host journal with
		// unbounded retention, and the set's own discord link identifies the
		// message if one ever needs reviewing.
		slog.Info("ingest: joining the author's open chain",
			"channel", channelID, "set", plan.metadata.SetId)
	}

	fillMessageDefaults(&plan.metadata, e.Message, *e.GuildID)

	outcome := runIngestion(plan.metadata, plan.items, plan.joinsSet)
	renderOutcomeToMessage(e, outcome)

	// A fresh set opens a chain for its author, so the bare messages that follow
	// join it. runIngestion took metadata by value and stamped the new set id on
	// its own copy, so the registry it wrote is where that copy can be read back.
	if !plan.joinsSet && outcome.created > 0 {
		if setMeta, found := lookupSet(e.MessageID.String()); found {
			openChain(channelID, authorID, setMeta)
		}
	}
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
}

// prepareIngestion decides whether the message should be ingested and, if so,
// assembles its metadata and media items. `ok` is false when the message isn't
// for us (no trigger, no media, or a metadata error already reported to the
// poster).
func prepareIngestion(e *events.MessageCreate) ingestPlan {
	none := ingestPlan{}
	msg := e.Message

	// Ingestion is confined to the allowed channels, whatever the trigger —
	// the bot must not re-upload media from anywhere else.
	if !allowedChannelIDs[e.ChannelID] {
		return none
	}

	// A message carrying BOTH a role ping and an @-mention of the bot is someone
	// re-announcing content that is already in the library: the pings are for
	// people, and ingesting again would duplicate the records. Checked before the
	// switch below, which would otherwise take the @-mention branch and ingest.
	//
	// An @-mention alone still ingests (with `key: value` metadata) and a role
	// ping alone still ingests passively — only the combination is a skip. The
	// cost is that "@bot + metadata lines + an incidental role ping so people see
	// it" used to work and now won't; the ⏭️ reaction is what keeps that
	// discoverable instead of mysterious.
	//
	// Returns before claimMessage, so the reaction is added here rather than
	// through the usual outcome rendering. Re-reacting on a gateway redelivery is
	// idempotent, so no dedupe is needed.
	if botIsMentioned(e) && len(msg.MentionRoles) > 0 {
		react(e, emojiSkipped)
		return none
	}

	switch {
	// Explicit @-mention.
	case botIsMentioned(e):
		items := collectMedia(msg)
		if len(items) == 0 {
			return none
		}
		metadata := Metadata{}
		if err := extractMetadata(msg.Content, &metadata); err != nil {
			// No reaction: same reasoning as ingestOutcome.emoji — a failure
			// marker on someone's post reads as a scolding. The trade is that a
			// malformed @-mention is now silent in-channel, so system_logs is
			// the only place it surfaces. The jump link is enough to review the
			// message; its full text is deliberately NOT stored — logs keep
			// derived data only, never verbatim user speech (retention promise
			// in the privacy policy).
			hooks.LogBotError("metadata parse failed: "+err.Error(), map[string]any{
				"discord": messageURL(e),
			})
			return none
		}
		return ingestPlan{metadata: metadata, items: items, ok: true}

	// Role ping: passive ingestion.
	case len(msg.MentionRoles) > 0:
		items := collectMedia(msg)
		if len(items) == 0 {
			return none
		}

		pingRoleNames, err := getRoleNamesFromIDs(e.Client(), *e.GuildID, msg.MentionRoles)
		if err != nil {
			slog.Error("unable to resolve role names", "err", err)
			return none
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
			return textDetection(e, items, pingRoleNames)
		}

		// Re-pinging the same roles is how plenty of people post one drop: three
		// messages in a row, each with its own ping, rather than a reply chain.
		// Taken literally that is three sets of the same thing, so a repeat ping
		// about the same subject joins the set the first one opened. It spends a
		// follow-up slot like any other continuation, so the same 3-message
		// budget applies.
		if open, ok := peekChain(e.ChannelID.String(), msg.Author.ID.String()); ok && sameSubject(open, metadata) {
			return ingestPlan{metadata: open, items: items, joinsSet: true, viaChain: true, ok: true}
		}
		return ingestPlan{metadata: metadata, items: items, ok: true}

	// Reply to a message that created a set: append to that set.
	case msg.ReferencedMessage != nil:
		remembered, found := lookupSet(msg.ReferencedMessage.ID.String())
		if found && remembered.AuthorID == msg.Author.ID.String() {
			items := collectMedia(msg)
			if len(items) == 0 {
				return none
			}
			return ingestPlan{metadata: remembered, items: items, joinsSet: true, ok: true}
		}
		// A reply to something else entirely can still be a plain follow-up in a
		// drop — people reply to their own earlier message just for threading —
		// so this falls through to the chain check rather than returning.
	}

	return chainContinuation(e)
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

// chainContinuation matches a bare follow-up: no trigger of its own, posted by
// the same author, in the same channel, shortly after a ping that opened a set.
//
// This is the loosest rule in the file and the only one that infers intent, so
// it is also the most conservative about what it will look at: anything with a
// trigger of its own has already been handled above, and both are re-checked
// here because those branches also bail on "no media" and "roles didn't parse",
// neither of which should quietly become a continuation of an earlier set.
func chainContinuation(e *events.MessageCreate) ingestPlan {
	none := ingestPlan{}
	msg := e.Message

	if botIsMentioned(e) || len(msg.MentionRoles) > 0 {
		return none
	}

	items := collectMedia(msg)
	if len(items) == 0 {
		return none
	}

	if meta, ok := peekChain(e.ChannelID.String(), msg.Author.ID.String()); ok {
		return ingestPlan{metadata: meta, items: items, joinsSet: true, viaChain: true, ok: true}
	}

	return textDetection(e, items, nil)
}

// textDetection is the last resort: no usable trigger, but the message names an
// idol the directory knows. See detect.go for why this exists and how hard it
// tries not to guess.
//
// `extraTerms` is scanned alongside the message text. Pinged role names go in
// there, so a group-only ping still contributes its group.
func textDetection(e *events.MessageCreate, items []MediaItem, extraTerms []string) ingestPlan {
	none := ingestPlan{}

	dir, err := loadDirectory()
	if err != nil {
		return none
	}
	content := e.Message.Content
	if len(extraTerms) > 0 {
		content += "\n" + strings.Join(extraTerms, "\n")
	}
	found, ok := detectSubjects(dir, content)
	if !ok {
		return none
	}

	// Logged for every hit, at warning level, on purpose: this is the only rule
	// that decides an attribution nobody stated, and the log is how its hit rate
	// and its mistakes get reviewed. Noise here is the point.
	hooks.LogBotWarning("ingested from message text (no ping)", map[string]any{
		"discord": messageURL(e),
		"idols":   strings.Join(found.idolNames, ", "),
		"groups":  strings.Join(found.groupNames, ", "),
		"matched": strings.Join(found.matched, ", "),
	})

	metadata := Metadata{
		Idol:  strings.Join(found.idolNames, ", "),
		Group: strings.Join(found.groupNames, ", "),
	}
	// `key: value` lines still win — someone who wrote them meant them.
	if err := extractMetadata(e.Message.Content, &metadata); err != nil {
		return none
	}
	return ingestPlan{metadata: metadata, items: items, ok: true}
}

// uploaderFromMessage decides who an ingested message is credited to, and
// reports which signal decided it.
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
func uploaderFromMessage(msg discord.Message) (name string, source string) {
	if md := msg.InteractionMetadata; md != nil && md.User.Username != "" {
		return md.User.Username, "interaction"
	}
	// The deprecated predecessor of the field above. Still populated on older
	// messages, and cheap to keep reading.
	if in := msg.Interaction; in != nil && in.User.Username != "" {
		return in.User.Username, "interaction-legacy"
	}
	return msg.Author.Username, "author"
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

// fillMessageDefaults stamps the message-level fields shared by every entry
// point. An explicit `uploader:` line wins over whatever uploaderFromMessage
// works out.
//
// For a relayed post that Discord attributes only to the app, the app's own
// "Posted by:" line is tried before settling on the app's name — see
// creditFromPostedBy. The interaction invoker still wins when Discord exposes
// one: it is the exact username, where the line is display text.
func fillMessageDefaults(metadata *Metadata, msg discord.Message, guildID snowflake.ID) {
	if metadata.Uploader == "" {
		name, source := uploaderFromMessage(msg)
		relayed := msg.Author.Bot || msg.WebhookID != nil
		if relayed && source == "author" {
			if credited, ok := creditFromPostedBy(msg); ok {
				name, source = credited, "posted-by"
			}
		}
		metadata.Uploader = name

		// Logged only for relayed posts, which are the ones that pile into one
		// bucket — and this line is how we find out whether a given relay app
		// leaves the invoker recoverable at all, from real traffic rather than by
		// reading its source. stdout, not system_logs: nothing has gone wrong.
		if relayed {
			slog.Info("ingest: crediting a relayed post",
				"uploader", name,
				"source", source,
				"app", msg.Author.Username,
				"webhook", msg.WebhookID != nil,
			)
		}
	}
	metadata.Discord = fmt.Sprintf("https://discord.com/channels/%s/%s/%s", guildID, msg.ChannelID, msg.ID)
	metadata.AuthorID = msg.Author.ID.String()
	metadata.MessageID = msg.ID.String()
}

// ─── Pipeline ────────────────────────────────────────────────────────────────

// ingestOutcome is what an ingestion run reports back to the poster.
type ingestOutcome struct {
	created  int
	total    int
	failures []string // "<filename> — <error>" per failed item
	missing  []string // idol/group names that didn't resolve
	fatal    string   // set when nothing was attempted at all
	// Set when the run was deliberately abandoned rather than attempted — a
	// blocked uploader. Distinct from `fatal`: nothing went wrong, so this must
	// not read as an error anywhere.
	skipped string
}

// emoji is the reaction marker for this outcome. Configurable — see config.go.
// Note this is the *reaction* form; ingestOutcome.text() keeps plain unicode
// because that string is message content, where custom emoji need the
// `<:name:id>` tag form instead.
//
// Only three outcomes are marked, and none of them is a complaint:
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
//   - A deliberate skip → ⏭️, the one "we didn't ingest this" worth making
//     visible, otherwise a blocked poster keeps re-posting into a void. Same
//     marker the ping+mention skip uses.
func (o ingestOutcome) emoji() string {
	switch {
	case o.skipped != "":
		return emojiSkipped
	case o.fatal != "" || o.created == 0:
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
func runIngestion(metadata Metadata, items []MediaItem, isReply bool) ingestOutcome {
	outcome := ingestOutcome{total: len(items)}

	rel, err := resolveRelations(metadata)
	if err != nil {
		slog.Error("relation resolution failed", "err", err)
		outcome.fatal = fmt.Sprintf("could not resolve names: %v", err)
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
		return outcome
	}

	// idol/group are required on both contents and sets — fail up front with
	// the actual unresolved names instead of per-item validation errors.
	if len(rel.idolIDs) == 0 || len(rel.groupIDs) == 0 {
		outcome.fatal = "no matching idol/group found in the database"
		if len(rel.missing) > 0 {
			outcome.fatal += " (unresolved: " + strings.Join(rel.missing, ", ") + ")"
		}
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
			return outcome
		}
		metadata.SetId = setId
		rememberSet(metadata.MessageID, metadata)
	}

	for _, item := range items {
		if _, err := createContentRecord(item, metadata, rel); err != nil {
			slog.Warn("unable to process media item", "url", item.URL, "err", err)
			outcome.failures = append(outcome.failures, fmt.Sprintf("%s — %v", item.Filename, err))
		} else {
			outcome.created++
		}
	}

	// AVIF-ready notifications fire separately from the R2 hook via
	// hooks.OnAvifReady once transcoding finishes; this is just the intake
	// receipt.
	slog.Info("ingestion finished", "message", metadata.MessageID, "created", outcome.created, "failed", len(outcome.failures))
	return outcome
}

// botIsMentioned checks for a literal <@id> mention in the message text.
// Deliberately content-based: a reply that pings the bot's message populates
// the mentions array but not the content, and must NOT count as a mention
// trigger.
func botIsMentioned(e *events.MessageCreate) bool {
	botID := e.Client().ApplicationID.String()
	return strings.Contains(e.Message.Content, "<@"+botID+">") ||
		strings.Contains(e.Message.Content, "<@!"+botID+">")
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

// ─── Poster feedback (message path) ──────────────────────────────────────────

// renderOutcomeToMessage reports the outcome on the triggering message. Only a
// reaction is left in the channel — the detail goes to the "system_logs"
// collection so the scraping channels stay free of bot chatter.
func renderOutcomeToMessage(e *events.MessageCreate, o ingestOutcome) {
	react(e, o.emoji())
	logOutcome(o, messageURL(e))
}

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
	// audit trail (see the note at the top of this function). The ⏭️ reaction is
	// what tells the poster.
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

// messageURL rebuilds the jump link for the triggering message, used as the
// context handle on a log entry.
func messageURL(e *events.MessageCreate) string {
	if e.GuildID == nil {
		return ""
	}
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", *e.GuildID, e.ChannelID, e.MessageID)
}

func react(e *events.MessageCreate, emoji string) {
	reactTo(e.Client(), e.ChannelID, e.MessageID, emoji)
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
