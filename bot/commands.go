package bot

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	disbot "github.com/disgoorg/disgo/bot"
	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/omit"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// Shared option vocabulary across the commands: `link` is always the primary
// URL argument, `format` always selects which derivative to hand back, and
// count-style options carry the same bounds on the Discord side as here.
const (
	maxUnwrapPerPage = 5
	// maxUnwrapItems bounds how much of a set /unwrap will page through. Every
	// page is held in memory for an hour per invocation (see PaginationState),
	// and no real set comes near this.
	maxUnwrapItems = 200
)

type PaginationState struct {
	Pages     []string
	Page      int
	CreatedAt time.Time
}

// paginationStates stores per-user pagination state keyed "userID:messageID",
// so users can't interfere with each other's pagination. Guarded by
// paginationMu — event handlers run concurrently.
var (
	paginationMu     sync.Mutex
	paginationStates = make(map[string]PaginationState)
)

func getPaginationKey(userID, messageID string) string {
	return fmt.Sprintf("%s:%s", userID, messageID)
}

func registerSlashCommands(client *disbot.Client) {
	commands := []discord.ApplicationCommandCreate{
		reviveCommandCreate(),
		discord.SlashCommandCreate{
			Name:        "match",
			Description: "Find the goyangi copy of a link.",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "link",
					Description: "An imgur link, a goyangi post link, or a cdn file link",
					Required:    true,
				},
			},
		},
		discord.SlashCommandCreate{
			Name:        "source",
			Description: "Get the video source (youtube link) for a piece of content.",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "link",
					Description: "An imgur link, a goyangi post link, or a cdn file link",
					Required:    true,
				},
			},
		},
		discord.SlashCommandCreate{
			Name:        "unwrap",
			Description: "Unwrap a Goyangi set link with interactive pagination",
			Options: []discord.ApplicationCommandOption{
				discord.ApplicationCommandOptionString{
					Name:        "link",
					Description: "A link like 'https://goyangi.pics/set/yv5dzbdxz04lap5'",
					Required:    true,
				},
				discord.ApplicationCommandOptionString{
					Name:        "format",
					Description: "Which file to link (default: mp4)",
					Choices:     unwrapFormatChoices,
				},
				discord.ApplicationCommandOptionInt{
					Name:        "per_page",
					Description: fmt.Sprintf("How many links to show per page (1‑%d, default 1)", maxUnwrapPerPage),
					MinValue:    ptr(1),
					MaxValue:    ptr(maxUnwrapPerPage),
				},
				discord.ApplicationCommandOptionBool{
					Name:        "show_metadata",
					Description: "Show metadata (idol, group, etc) on the first page (hidden by default).",
				},
			},
		},
		postCommandCreate(),
		convertCommandCreate(),
		discord.SlashCommandCreate{
			Name:        "random",
			Description: "A random piece of content from the library.",
			Options:     discoveryOptions(),
		},
		discord.SlashCommandCreate{
			Name:        "top",
			Description: "The most-liked content in a period.",
			Options:     discoveryOptions(),
		},
		// Discord's Developer ToS §5 requires the privacy policy and a data
		// deletion route to be easily accessible from the app itself, not just
		// from the website it feeds.
		discord.SlashCommandCreate{
			Name:        "privacy",
			Description: "Privacy policy, data deletion and ingestion opt-out.",
		},
		showCommandCreate(),
		// For one-offs in social/group channels. Not confined to
		// DISCORD_ALLOWED_CHANNEL_IDS; gated by role, see manualIngestRoleIDs.
		reuploadCommandCreate(),
		// Right-click any message → Apps → runs the ingestion pipeline on it.
		// Covers posts from before the bot existed or without a role ping.
		//
		// Works in any channel, so it carries the same two gates /reupload does:
		// this permission hides it from regular members, and manualIngestRoleIDs
		// is the enforced check behind it.
		discord.MessageCommandCreate{
			Name:                     "Ingest this message",
			DefaultMemberPermissions: omit.NewPtr(discord.PermissionManageMessages),
		},
	}

	// Registered globally, so the commands exist in every guild the bot is
	// invited to with no per-guild allowlist to maintain. Global commands can
	// take a moment to propagate to clients, unlike guild commands.
	//
	// Degrade gracefully — a transient Discord API error must not take down the
	// whole PocketBase process.
	if _, err := client.Rest.SetGlobalCommands(client.ApplicationID, commands); err != nil {
		slog.Error("cannot register slash commands", "err", err)
	}
}

// onCommand dispatches slash and message (context-menu) commands.
func onCommand(e *events.ApplicationCommandInteractionCreate) {
	defer recoverHandler("command")

	switch data := e.Data.(type) {
	case discord.SlashCommandInteractionData:
		switch data.CommandName() {
		case "revive":
			handleReviveCommand(e, data)
		case "match":
			handleMatchCommand(e, data)
		case "unwrap":
			handleUnwrapCommand(e, data)
		case "post":
			handlePostCommand(e, data)
		case "source":
			handleSourceCommand(e, data)
		case "convert":
			handleConvertCommand(e, data)
		case "random":
			handleRandomCommand(e, data)
		case "top":
			handleTopCommand(e, data)
		case "reupload":
			handleReuploadCommand(e, data)
		case "show":
			handleShowCommand(e, data)
		case "privacy":
			handlePrivacyCommand(e)
		}
	case discord.MessageCommandInteractionData:
		if data.CommandName() == "Ingest this message" {
			handleIngestContext(e, data)
		}
	}
}

// onModalSubmit dispatches modal submissions.
//
// Only the context menu uses a modal, and its custom id carries the target
// message, so the prefix is both the routing key and the payload. Anything else
// is ignored rather than answered — an unknown custom id is either a stale modal
// from an older build or something we did not send.
func onModalSubmit(e *events.ModalSubmitInteractionCreate) {
	defer recoverHandler("modal")

	if strings.HasPrefix(e.Data.CustomID, ingestModalPrefix) {
		handleIngestModal(e)
	}
}

// onComponent dispatches the pagination buttons.
func onComponent(e *events.ComponentInteractionCreate) {
	defer recoverHandler("component")

	data, ok := e.Data.(discord.ButtonInteractionData)
	if !ok {
		return
	}
	customID := data.CustomID()

	// /show's buttons carry their record id in the custom id, so they match on a
	// prefix rather than an exact name.
	if strings.HasPrefix(customID, showButtonPrefix+":") {
		handleShowFormatInteraction(e, customID)
		return
	}

	switch customID {
	case "first", "prev", "next", "last":
		handlePaginationInteraction(e, customID)
	}
}

// optString reads an optional string option, treating an empty value the same
// as an absent one.
func optString(data discord.SlashCommandInteractionData, name, fallback string) string {
	if v, ok := data.OptString(name); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

// ptr is for the *int bounds on integer command options.
func ptr[T any](v T) *T {
	return &v
}

// normalizeImgurOption converts a bare imgur.com page link into the direct
// i.imgur.com .mp4 form that "mirror" fields are stored in.
func normalizeImgurOption(link string) string {
	if strings.HasPrefix(link, "https://imgur.com/") {
		return strings.Replace(link, "https://imgur.com/", "https://i.imgur.com/", 1) + ".mp4"
	}
	return link
}

// pathSegmentAfter returns the URL path segment following the given one, e.g.
// "abc" for ("https://goyangi.pics/set/abc?page=2", "set"). Query strings,
// fragments, trailing slashes and percent-escapes are all handled — people
// paste links straight out of the browser.
func pathSegmentAfter(raw, want string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false
	}
	segments := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	for i, segment := range segments {
		if segment == want && i+1 < len(segments) && segments[i+1] != "" {
			return segments[i+1], true
		}
	}
	return "", false
}

// setRef is a parsed goyangi set or collection link.
type setRef struct {
	kind   string // "set" or "collection"
	id     string
	filter string // matches the contents in it, with the id bound as {:id}
}

// params binds the id for the filter.
func (s setRef) params() dbx.Params {
	return dbx.Params{"id": s.id}
}

// pageLink rebuilds the canonical goyangi URL, so a link pasted with a query
// string or from a different host still posts as a clean one.
func (s setRef) pageLink() string {
	return fmt.Sprintf("%s/%s/%s", publicBaseURL, s.kind, s.id)
}

// parseSetLink pulls the record id out of a goyangi set or collection link and
// builds the filter that matches its contents.
//
// Note the ids these carry are NOT the usual opaque PocketBase ids — the R2
// hook builds them as date-group-idol-suffix (see hooks.generateContentId), so
// they contain hyphens and whatever punctuation a group or idol name holds.
// The id is bound as a query parameter rather than interpolated, so it needs no
// validation here beyond being present.
func parseSetLink(raw string) (setRef, error) {
	if id, ok := pathSegmentAfter(raw, "set"); ok {
		// strict match on the single-value "set" field
		return setRef{kind: "set", id: id, filter: "set = {:id}"}, nil
	}
	if id, ok := pathSegmentAfter(raw, "collection"); ok {
		// "collections" is an array → use ~ to match if the id is present
		return setRef{kind: "collection", id: id, filter: "collections ~ {:id}"}, nil
	}
	return setRef{}, fmt.Errorf("link must contain `/set/<id>` or `/collection/<id>`")
}

// findSetContents returns the items in a set or collection, newest first. The
// first item is what the set page uses as its cover.
func findSetContents(ref setRef) ([]*core.Record, error) {
	return App.FindRecordsByFilter("contents", ref.filter, "-created", maxUnwrapItems, 0, ref.params())
}

// resolveContentRecord finds the content record a link refers to, accepting any
// shape a link to an item can take: a goyangi content page (/single/<id>), the
// imgur mirror, the R2 mp4, or the animated preview. Returns nil without error
// when nothing matches.
func resolveContentRecord(link string) (*core.Record, error) {
	link = strings.TrimSpace(link)
	if link == "" {
		return nil, nil
	}

	// A content page link carries the record id in its path.
	if id, ok := pathSegmentAfter(link, "single"); ok {
		if record, err := App.FindRecordById("contents", id); err == nil {
			return record, nil
		}
		// A miss here isn't fatal — fall through and try the URL fields, since
		// /single/<id> isn't the only link shape that reaches this point.
	}

	// Otherwise match against every URL we store for an item.
	records, err := App.FindRecordsByFilter("contents",
		"mirror = {:link} || original = {:link} || preview = {:link}", "", 1, 0,
		dbx.Params{"link": normalizeImgurOption(link)})
	if err != nil || len(records) == 0 {
		return nil, err
	}
	return records[0], nil
}

// buildFileLink builds the public URL of a PocketBase-hosted file.
func buildFileLink(recordID, filename string) string {
	return fmt.Sprintf("%s/v1/%s/%s", publicBaseURL, recordID, filename)
}

// handleMatchCommand looks an imgur link up in the library and returns our own
// copy of it — the MP4 in R2, falling back to the PocketBase-hosted file for a
// record whose upload hook hasn't finished. /revive is the counterpart for
// links we don't have.
func handleMatchCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	link := data.String("link")

	r := newReply(e)
	if !r.Defer() {
		return
	}

	record, err := resolveContentRecord(link)
	if err != nil {
		slog.Error("match: lookup failed", "err", err)
		r.Fail("Could not query the library.")
		return
	}
	if record == nil {
		r.Fail("No goyangi record matches that link. Try `/revive` to re-encode it instead.")
		return
	}

	mp4 := mediaLink(record, formatMP4)
	if mp4 == "" {
		r.Fail("Found a matching record but it has no usable file.")
		return
	}

	r.Publish(discord.NewMessageCreate().WithContentf("[%s](%s)\n%s",
		discordSafeTitle(record.GetString("title")), contentPageLink(record.Id), mp4))
}

func handleSourceCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	link := data.String("link")

	r := newReply(e)
	if !r.Defer() {
		return
	}

	record, err := resolveContentRecord(link)
	if err != nil {
		slog.Error("source: lookup failed", "err", err)
		r.Fail("Could not query the library.")
		return
	}
	if record == nil {
		r.Fail("No goyangi record matches that link.")
		return
	}

	source := record.GetString("source")
	if source == "" {
		r.Fail("No source was recorded for that gif.")
		return
	}

	r.Publish(discord.NewMessageCreate().WithContentf("🔗 Found source in Goyangi: %s", source))
}

// handleUnwrapCommand expands a set or collection link into a paginated list
// of media links.
func handleUnwrapCommand(e *events.ApplicationCommandInteractionCreate, data discord.SlashCommandInteractionData) {
	setLink := strings.TrimSpace(data.String("link"))
	format := optString(data, "format", formatMP4)
	showMetadata := data.Bool("show_metadata") // hidden unless explicitly requested
	perPage := min(max(data.Int("per_page"), 1), maxUnwrapPerPage)

	r := newReply(e)
	if !r.Defer() {
		return
	}

	ref, err := parseSetLink(setLink)
	if err != nil {
		r.Fail("%s", err.Error())
		return
	}

	records, err := findSetContents(ref)
	if err != nil {
		slog.Error("unwrap: lookup failed", "err", err)
		r.Fail("Could not query the library.")
		return
	}
	if len(records) == 0 {
		r.Fail("No items found for that set.")
		return
	}

	links := make([]string, 0, len(records))
	for _, record := range records {
		if link := mediaLink(record, format); link != "" {
			links = append(links, link)
		}
	}
	if len(links) == 0 {
		r.Fail("No usable %s links found for that set.", format)
		return
	}

	pages := make([]string, 0, (len(links)+perPage-1)/perPage)
	for idx := 0; idx < len(links); idx += perPage {
		chunk := strings.Join(links[idx:min(idx+perPage, len(links))], "\n")
		if idx == 0 && showMetadata {
			chunk = buildMetaHeader(records[0]) + "\n" + chunk
		}
		pages = append(pages, chunk)
	}

	sendPaginatedResponse(r, pages, 0)
}

// buildMetaHeader renders the title/groups/idols/uploader block shown above the
// links on the first page. Relations are expanded here rather than in the
// lookup because the block is opt-in — the extra queries only run when it's
// actually asked for.
func buildMetaHeader(record *core.Record) string {
	if errs := App.ExpandRecord(record, []string{"idol", "group", "uploader"}, nil); len(errs) > 0 {
		slog.Warn("unwrap: could not expand relations", "record", record.Id, "errs", fmt.Sprint(errs))
	}

	return fmt.Sprintf(
		"**Title**: %s\n**Created**: %s\n**Groups**: %s\n**Idols**: %s\n**Uploader**: %s\n\n**Links**:",
		discordSafeTitle(record.GetString("title")),
		record.GetDateTime("created").Time().UTC().Format("2006-01-02 15:04 UTC"),
		joinNames(record.ExpandedAll("group")),
		joinNames(record.ExpandedAll("idol")),
		joinNames(record.ExpandedAll("uploader")),
	)
}

// joinNames lists the "name" field of expanded relation records.
func joinNames(records []*core.Record) string {
	names := make([]string, 0, len(records))
	for _, record := range records {
		if name := strings.TrimSpace(record.GetString("name")); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return "—"
	}
	return strings.Join(names, ", ")
}

func buildPaginationContent(pages []string, page int) (string, discord.ActionRowComponent) {
	content := fmt.Sprintf("**Page %d / %d**\n\n%s", page+1, len(pages), pages[page])

	row := discord.NewActionRow(
		discord.NewPrimaryButton("", "first").WithEmoji(discord.ComponentEmoji{Name: "⏮️"}),
		discord.NewPrimaryButton("", "prev").WithEmoji(discord.ComponentEmoji{Name: "⬅️"}),
		discord.NewPrimaryButton("", "next").WithEmoji(discord.ComponentEmoji{Name: "➡️"}),
		discord.NewPrimaryButton("", "last").WithEmoji(discord.ComponentEmoji{Name: "⏭️"}),
	)

	return content, row
}

// sendPaginatedResponse posts the first page and remembers the pages against
// the message it landed on, so the buttons have something to page through.
func sendPaginatedResponse(r *reply, pages []string, page int) {
	content, row := buildPaginationContent(pages, page)

	msg := r.Publish(discord.NewMessageCreate().WithContent(content).WithComponents(row))
	if msg == nil {
		return
	}

	paginationMu.Lock()
	defer paginationMu.Unlock()
	paginationStates[getPaginationKey(r.e.User().ID.String(), msg.ID.String())] = PaginationState{
		Pages:     pages,
		Page:      page,
		CreatedAt: time.Now(),
	}
	cleanupOldPaginationStatesLocked()
}

func handlePaginationInteraction(e *events.ComponentInteractionCreate, action string) {
	key := getPaginationKey(e.User().ID.String(), e.Message.ID.String())

	paginationMu.Lock()
	state, ok := paginationStates[key]
	if !ok {
		paginationMu.Unlock()
		if err := e.CreateMessage(discord.NewMessageCreate().
			WithEphemeral(true).
			WithContent("Pagination state not found for this user."),
		); err != nil {
			slog.Error("pagination: could not respond", "err", err)
		}
		return
	}

	switch action {
	case "first":
		state.Page = 0
	case "prev":
		state.Page = max(state.Page-1, 0)
	case "next":
		state.Page = min(state.Page+1, len(state.Pages)-1)
	case "last":
		state.Page = len(state.Pages) - 1
	}

	paginationStates[key] = state
	paginationMu.Unlock()

	content, row := buildPaginationContent(state.Pages, state.Page)
	if err := e.UpdateMessage(discord.NewMessageUpdate().WithContent(content).WithComponents(row)); err != nil {
		slog.Error("pagination: could not update message", "err", err)
	}
}

// cleanupOldPaginationStatesLocked removes pagination states older than 1 hour
// to prevent unbounded growth. Caller must hold paginationMu.
func cleanupOldPaginationStatesLocked() {
	cutoff := time.Now().Add(-1 * time.Hour)
	for key, state := range paginationStates {
		if state.CreatedAt.Before(cutoff) {
			delete(paginationStates, key)
		}
	}
}

// handlePrivacyCommand answers /privacy with the policy links and the
// deletion/opt-out contact. Static and ephemeral — the point is that anyone
// whose posts the bot may archive can find out what happens to them and how to
// say no, without leaving Discord.
func handlePrivacyCommand(e *events.ApplicationCommandInteractionCreate) {
	respondEphemeral(e, fmt.Sprintf(
		"**Privacy & your data**\n"+
			"Media posted in the archive channels may be saved to %[1]s and publicly credited to your Discord username.\n"+
			"Privacy policy: %[1]s/privacy\n"+
			"Data deletion & ingestion opt-out: %[1]s/takedown or %[2]s",
		publicBaseURL, supportEmail,
	))
}

// respondEphemeral sends a reply only visible to the invoking user. Used by the
// handlers that reject before doing any work; everything else goes through
// reply (reply.go).
func respondEphemeral(e *events.ApplicationCommandInteractionCreate, msg string) {
	if err := e.CreateMessage(discord.NewMessageCreate().WithEphemeral(true).WithContent(msg)); err != nil {
		slog.Error("could not respond to interaction", "err", err)
	}
}
