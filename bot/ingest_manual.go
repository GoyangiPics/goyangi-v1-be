package bot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// "Ingest this message" for a bot that forwards over the ingest API: the same
// thing our own bot offers as the context menu and /reupload, asked for through
// the other bot's UI.
//
//	POST /api/ingest/discord/manual        ask
//	GET  /api/ingest/discord/manual/{id}   the answer, after a 202
//
// Everything after the question runs here, on the code our commands use: the
// role gate (DISCORD_INGEST_ROLE_IDS), the upload-permission check
// (callerUploadDenial), idol resolution, the set link, then manualIngestCore —
// the caller credited, the message's own date, the claim. The other bot only
// asks the question and shows the answer.
//
// Like our commands, it works in any channel: a person named one message and
// asked for it, so the allowlist doesn't apply and the gates above do. Two of
// those gates are stricter here than on our commands, because our commands
// have something this route doesn't — Discord itself, hiding the command from
// members without permission and vouching for who ran it:
//
//   - the role gate fails closed: with DISCORD_INGEST_ROLE_IDS unset, nobody
//     may ask;
//   - the person is matched on their Discord username, which is unique, and
//     not on their display name, which anyone can set to anything.
//
// And the message must come from a server listed in INGEST_GUILD_IDS. The
// answer carries `reply`, the text our bot would have shown the person.

// manualJobPrefix keeps manual answers apart from passive verdicts for the same
// message in the job registry.
const manualJobPrefix = "manual:"

// manualAPIRequest is one manual request.
type manualAPIRequest struct {
	// The message to archive, as Discord delivered it, guild_id included. A
	// message command's target (the interaction's resolved message) lacks
	// guild_id; add the interaction's.
	Message   json.RawMessage `json:"message"`
	BotUserID string          `json:"bot_user_id"`
	// Who asked: credited as the uploader, and checked like our commands check
	// the person running them.
	RequestedBy *struct {
		ID       string   `json:"id"`
		Username string   `json:"username"`
		Roles    []string `json:"roles"`
	} `json:"requested_by"`
	// Idol names, as typed or picked. "Name — Group" disambiguates a name
	// shared by several groups; record ids work too.
	Idols []string `json:"idols"`
	Tags  []string `json:"tags"`
	// Optional: a goyangi set link to add the items to.
	Set    string `json:"set"`
	DryRun bool   `json:"dry_run"`
}

// manualAsk is a validated manual request.
type manualAsk struct {
	msg      discord.Message
	guildID  snowflake.ID
	username string // the requester's Discord username
	roles    []snowflake.ID
	idolsRaw string
	tagsRaw  string
	setRaw   string
}

func handleManualPost(e *core.RequestEvent) error {
	if isDraining() {
		e.Response.Header().Set("Retry-After", "60")
		return e.Error(http.StatusServiceUnavailable, "goyangi is restarting; retry in a minute.", nil)
	}

	var req manualAPIRequest
	dec := json.NewDecoder(e.Request.Body)
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, apis.ErrRequestEntityTooLarge) {
			return apis.ErrRequestEntityTooLarge
		}
		return e.BadRequestError("Body must be JSON: {message, requested_by, idols, bot_user_id} ("+err.Error()+").", nil)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return e.BadRequestError("Body must be a single JSON object.", nil)
	}
	ask, door, err := req.validate()
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	id := ask.msg.ID.String()

	if req.DryRun {
		resp := runManual(ask, true)
		resp.DryRun, resp.Reaction = true, ""
		return e.JSON(http.StatusOK, resp)
	}
	// A dry run changes nothing, not even which bots goyangi knows.
	rememberForwarder(door)
	job, ok, _ := startJob(manualJobPrefix+id, nil, func() ingestResponse { return runManual(ask, false) })
	return answerJob(e, id, job, ok, ingestWait)
}

func (req manualAPIRequest) validate() (ask manualAsk, door snowflake.ID, err error) {
	msg, err := forwardedMessage(req.Message)
	if err != nil {
		return ask, 0, err
	}
	if door, err = forwarderID(req.BotUserID); err != nil {
		return ask, 0, err
	}
	rb := req.RequestedBy
	if rb == nil || strings.TrimSpace(rb.Username) == "" {
		return ask, 0, fmt.Errorf("`requested_by` needs the `id` and `username` of the person who asked")
	}
	if uid, perr := snowflake.Parse(strings.TrimSpace(rb.ID)); perr != nil || uid == 0 {
		return ask, 0, fmt.Errorf("`requested_by.id` isn't a Discord user id")
	}
	var roles []snowflake.ID
	for _, raw := range rb.Roles {
		rid, perr := snowflake.Parse(strings.TrimSpace(raw))
		if perr != nil {
			return ask, 0, fmt.Errorf("`requested_by.roles` must be role ids, got %q", raw)
		}
		roles = append(roles, rid)
	}

	return manualAsk{
		msg:      msg,
		guildID:  *msg.GuildID,
		username: strings.TrimSpace(rb.Username),
		roles:    roles,
		idolsRaw: strings.Join(req.Idols, ", "),
		tagsRaw:  strings.Join(req.Tags, ", "),
		setRaw:   req.Set,
	}, door, nil
}

// runManual answers one manual request: the gates our commands apply, in the
// same order, then manualIngestCore. A dry run stops before anything is saved.
func runManual(a manualAsk, dryRun bool) ingestResponse {
	id := a.msg.ID.String()
	refuse := func(status ingestStatus, code, text string) ingestResponse {
		if status == statusFailed {
			text = "❌ " + text // as our commands' Fail shows it
		}
		return ingestResponse{MessageID: id, Status: string(status), Reason: code, Reply: text}
	}

	if !manualGuildIDs[a.guildID] {
		return refuse(statusDenied, reasonServerNotListed, "goyangi doesn't archive from this server.")
	}
	if !requesterMayIngest(a.roles) {
		return refuse(statusDenied, reasonNoPermission, "You don't have permission to use this command.")
	}
	denial, uploaderName := callerUploadDenial(a.username)
	if denial != "" {
		return refuse(statusDenied, reasonCannotUpload, string(denial))
	}
	joinSetID, refusal := resolveJoinSet(a.setRaw)
	if refusal != "" {
		return refuse(statusFailed, reasonBadSet, refusal)
	}
	// Other bots' posts are ingestible — the forwarding bot's included, it may
	// post for people — but our own bot's output is not.
	if a.msg.Author.ID == snowflake.ID(ourBotID.Load()) {
		return refuse(statusFailed, reasonOwnMessage, "The bot's own messages can't be ingested.")
	}
	// Already on the site — archived automatically, or asked for before. Our
	// commands would archive it again; here the person is told to edit the post
	// instead, so a second copy never appears.
	link := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", a.guildID, a.msg.ChannelID, a.msg.ID)
	if alreadyIngested(link) {
		return refuse(statusDuplicate, reasonAlreadyArchived, "That message is already on goyangi — edit the post on the site instead.")
	}
	dir, err := loadDirectory()
	if err != nil {
		return refuse(statusFailed, reasonInternal, "Could not load the idol/group directory. Try again shortly.")
	}
	idolNames, groupNames, unknown := resolveReuploadIdols(dir, a.idolsRaw)
	if len(unknown) > 0 {
		resp := refuse(statusFailed, reasonUnknownIdols, fmt.Sprintf("I don't know: %s.", strings.Join(unknown, ", ")))
		resp.Unresolved = unknown
		return resp
	}
	if len(idolNames) == 0 {
		return refuse(statusFailed, reasonNoIdols, "Pick at least one idol.")
	}

	req := manualIngest{idolNames: idolNames, groupNames: groupNames, tagsRaw: a.tagsRaw, uploader: uploaderName}
	res := manualIngestCore(a.msg, a.guildID, req, joinSetID, dryRun)
	if dryRun && res.refusalCode == reasonAlreadyHandled {
		resp := manualPreview(id, res, joinSetID)
		resp.Status, resp.Reason, resp.Reply = string(statusDuplicate), reasonAlreadyHandled, res.refusal
		return resp
	}
	if res.refusalCode != "" {
		status := statusFailed
		if res.refusalCode == reasonAlreadyHandled {
			status = statusDuplicate
		}
		return refuse(status, res.refusalCode, res.refusal)
	}
	if dryRun {
		return manualPreview(id, res, joinSetID)
	}

	o := res.outcome
	result := ingestResult{status: statusFailed, reason: reasonNothingSaved, outcome: o}
	switch {
	case o.skipped != "":
		result.status, result.reason = statusSkipped, o.skipCode
	case o.fatal != "":
		result.reason = o.fatalCode
	case o.created > 0:
		result.status, result.reason = statusIngested, ""
		result.setID = joinSetID
		if result.setID == "" {
			if setMeta, found := lookupSet(id); found {
				result.setID = setMeta.SetId
			}
		}
	}
	resp := responseFor(id, result)
	resp.Reply = manualReplyText(o)
	return resp
}

// manualGuildIDs are the servers manual requests may archive from, from
// INGEST_GUILD_IDS. A forwarding bot may sit in other servers too, and a
// person holding the right roles in one mustn't be able to pull messages from
// another. Unset: no manual requests at all. A var for the tests.
var manualGuildIDs = func() map[snowflake.ID]bool {
	m := map[snowflake.ID]bool{}
	for _, id := range csvSnowflakeEnv("INGEST_GUILD_IDS", "") {
		m[id] = true
	}
	return m
}()

// requesterMayIngest is callerMayIngestAnywhere for a requester whose roles
// the forwarding bot reported. Unlike our commands it fails CLOSED: with no
// roles configured there is no Discord-side permission hint to fall back on.
func requesterMayIngest(roles []snowflake.ID) bool {
	for _, r := range roles {
		if manualIngestRoleIDs[r] {
			return true
		}
	}
	return false
}

// manualReplyText is ingestOutcome.text for another developer's bot: the same
// messages, without the raw errors behind failed items (those stay in
// system_logs).
func manualReplyText(o ingestOutcome) string {
	switch {
	case o.skipped != "":
		return "⏭️ " + o.skipped
	case o.fatalCode == reasonUnresolved:
		return "❌ " + o.fatal
	case o.fatal != "":
		return "❌ Something went wrong on goyangi's side. Try again shortly."
	case o.created == 0:
		return "❌ No items could be ingested: " + strings.Join(o.failedItems, ", ")
	case len(o.failedItems) > 0:
		return fmt.Sprintf("⚠️ Ingested %d/%d items. Failed: %s", o.created, o.total, strings.Join(o.failedItems, ", "))
	default:
		return fmt.Sprintf("✅ Ingested %d item(s).", o.created)
	}
}

// manualPreview is a manual dry run's answer: what would be archived, and
// whether a block on the requester would stop it.
func manualPreview(id string, res manualResult, joinSetID string) ingestResponse {
	m := res.metadata
	idolIDs, groupIDs, missing := resolveSubjects(m)
	preview := &ingestPreview{
		Trigger:    "manual",
		Uploader:   m.Uploader,
		Title:      m.Title,
		Idols:      namesByIDs("groups_idols", idolIDs),
		Groups:     namesByIDs("groups", groupIDs),
		Unresolved: missing,
		Tags:       splitTrim(m.Tags),
	}
	if joinSetID != "" {
		preview.JoinsSet = publicBaseURL + "/set/" + joinSetID
	}
	preview.Title = previewTitle(m, idolIDs, groupIDs)
	preview.Items = previewItems(res.items, m.Filetype)
	resp := ingestResponse{MessageID: id, Status: string(statusIngested), Preview: preview, Total: len(res.items),
		Reply: fmt.Sprintf("Would ingest %d item(s), credited to %s.", len(res.items), m.Uploader)}
	if rec, err := findUploaderByNames(m.Uploader); err == nil && rec != nil && rec.GetBool("blockIngest") {
		resp.Status, resp.Reason = string(statusSkipped), reasonBlocked
		resp.Reply = "⏭️ Not ingested — " + m.Uploader + " is blocked from ingestion."
	}
	return resp
}
