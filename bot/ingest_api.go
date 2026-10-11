package bot

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"goyangi-v1-be/hooks"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// The ingest API: the passive triggers (1–3 in ingest.go), fed by another
// Discord bot instead of our own gateway connection.
//
// It exists so goyangi doesn't need its own bot in every server it archives. A
// bot that already reads those channels (Message Content intent) forwards each
// message as Discord delivered it, and everything after that — the channel
// allowlist, role-ping and @-mention triggers, replies and follow-up chains,
// text detection, imgur album resolution, relay credits, uploader matching,
// opt-outs and blocks — runs here, on the same code our own bot uses. The
// forwarding bot decides nothing, so the two can't drift apart.
//
// Contract, for whoever writes the forwarding side: docs/ingest-api.md.
//
//	POST /api/ingest/discord/messages        offer one message
//	GET  /api/ingest/discord/messages/{id}   the verdict on one
//	POST /api/ingest/discord/manual          "Ingest this message", asked for
//	GET  /api/ingest/discord/manual/{id}     the answer to one (ingest_manual.go)
//
// Both need `Authorization: Bearer <INGEST_API_TOKEN>`. Without the variable set
// the routes don't exist.
//
// A message can take a while: every item is downloaded before its record is
// created, a channel's messages are handled one at a time (state.go), and the
// API sits behind Cloudflare, which drops a request after 100 seconds. So the
// POST waits up to ingestWait for the verdict and otherwise answers 202
// "processing", with the GET to collect it later. The work is never tied to the
// request: a dropped connection doesn't cancel it.

// ingestWait is how long a POST holds out for the verdict before answering 202.
// Well inside Cloudflare's 100-second limit. A var for the tests.
var ingestWait = 25 * time.Second

// ingestJobTTL is how long a finished verdict stays collectable, and how long a
// repeated POST gets it back rather than a second run.
const ingestJobTTL = time.Hour

// maxIngestBody bounds the request. A Discord message object is a few KB even
// with embeds; nothing legitimate comes near this.
const maxIngestBody = 1 << 20

// maxPendingIngest bounds the jobs waiting or running. Past it the API answers
// 503 with Retry-After rather than queueing without limit. A var for the tests.
var maxPendingIngest = 200

// Discord's own limits on one message. Anything past them isn't a message
// Discord sent, and each attachment and link is a download, so they are also
// what bounds the work one request can cause.
const (
	maxMessageAttachments = 10
	maxMessageContent     = 4000 // runes
	maxMessageEmbeds      = 10
)

// discordAttachmentHosts are where Discord serves attachments from. A forwarded
// attachment pointing anywhere else isn't a Discord attachment.
var discordAttachmentHosts = map[string]bool{
	"cdn.discordapp.com":   true,
	"media.discordapp.net": true,
}

// isDraining reports a supervised restart in progress. A var for the tests.
var isDraining = hooks.Draining

// queuedWait is how long a POST holds out when its message is waiting behind
// others in its channel. Short, so a client sending a channel's messages one at
// a time isn't held up for each one; the answer comes later from the GET.
var queuedWait = 3 * time.Second

// ingestBotIDs are the bots allowed to forward, from INGEST_BOT_USER_IDS
// (comma-separated). Pinned in config so they're known from boot — our own bot
// leaves their posts to the API even before their first request after a
// restart — and so a wrong bot_user_id is refused rather than trusted: every
// id a request names becomes "one of goyangi's bots", whose posts are never
// content. Unset: any bot_user_id is accepted, and becomes known when it first
// forwards. A var for the tests.
var ingestBotIDs = func() map[snowflake.ID]bool {
	m := map[snowflake.ID]bool{}
	for _, id := range csvSnowflakeEnv("INGEST_BOT_USER_IDS", "") {
		m[id] = true
	}
	return m
}()

// ingestTokens are the accepted bearer tokens. Comma-separated, so a token can
// be rotated without downtime: add the new one, switch the bot, drop the old.
func ingestTokens() []string {
	var out []string
	for _, t := range strings.Split(os.Getenv("INGEST_API_TOKEN"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// RegisterIngestAPI adds the ingest routes, when INGEST_API_TOKEN is set.
// Independent of DISCORD_BOT_ENABLED: both doors can be open at once, and a
// message seen by both is ingested once.
func RegisterIngestAPI(app core.App) {
	tokens := ingestTokens()
	if len(tokens) == 0 {
		return
	}
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		registerIngestRoutes(e, tokens)
		return e.Next()
	})
	for id := range ingestBotIDs {
		rememberForwarder(id)
	}
	slog.Info("ingest API enabled", "tokens", len(tokens), "bots", len(ingestBotIDs))
}

func registerIngestRoutes(e *core.ServeEvent, tokens []string) {
	g := e.Router.Group("/api/ingest/discord")
	g.BindFunc(requireIngestToken(tokens))
	g.POST("/messages", handleIngestPost).Bind(apis.BodyLimit(maxIngestBody))
	g.GET("/messages/{id}", jobStatusHandler(""))
	g.POST("/manual", handleManualPost).Bind(apis.BodyLimit(maxIngestBody))
	g.GET("/manual/{id}", jobStatusHandler(manualJobPrefix))
}

// requireIngestToken checks the bearer token in constant time. A route-level
// check rather than a PocketBase auth record: the caller is a bot, not a user,
// and the token grants exactly this and nothing else.
func requireIngestToken(tokens []string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		header := e.Request.Header.Get("Authorization")
		if len(header) > len("Bearer ") && strings.EqualFold(header[:len("Bearer ")], "Bearer ") {
			got := []byte(strings.TrimSpace(header[len("Bearer "):]))
			for _, t := range tokens {
				if subtle.ConstantTimeCompare(got, []byte(t)) == 1 {
					return e.Next()
				}
			}
		}
		return e.UnauthorizedError("Missing or invalid ingest token.", nil)
	}
}

// ingestRequest is one forwarded message.
type ingestRequest struct {
	// The message object exactly as Discord delivered it (MESSAGE_CREATE's `d`),
	// guild_id included.
	Message json.RawMessage `json:"message"`
	// Names of the roles the message pings, by role id. Discord's message
	// object carries only the ids, and role names are where idol and group come
	// from ("Yujin [IVE]").
	Roles map[string]string `json:"roles"`
	// The forwarding bot's own user id. Required: it is how goyangi knows the
	// bot's own posts and the @-mentions meant for it.
	BotUserID string `json:"bot_user_id"`
	// Set on a message the forwarding bot posted on someone's behalf.
	Relay *ingestRelay `json:"relay"`
	// Decide everything, save nothing.
	DryRun bool `json:"dry_run"`
}

// ingestRelay is whose post a relayed message is: a person, or anonymous.
type ingestRelay struct {
	Anonymous bool `json:"anonymous"`
	User      *struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
}

// ingestResponse is the verdict, as JSON.
type ingestResponse struct {
	MessageID string `json:"message_id"`
	// ignored | duplicate | skipped | ingested | failed | processing
	Status string `json:"status"`
	// A stable code (ingest.go's reason* constants); see docs/ingest-api.md.
	Reason string `json:"reason,omitempty"`
	// The marker to leave on the message: "success", "skipped" or "" for none.
	Reaction string `json:"reaction"`
	Created  int    `json:"created"`
	Total    int    `json:"total"`
	// Filenames of the items that couldn't be saved.
	Failed []string `json:"failed,omitempty"`
	// Names that matched nothing: "idol X", "group Y".
	Unresolved []string `json:"unresolved,omitempty"`
	Set        string   `json:"set,omitempty"`
	// For a manual request: what to tell the person who asked, ready to show.
	Reply string `json:"reply,omitempty"`
	// Problems with the request that didn't stop it, such as a pinged role
	// sent without a name.
	Warnings []string       `json:"warnings,omitempty"`
	DryRun   bool           `json:"dry_run,omitempty"`
	Preview  *ingestPreview `json:"preview,omitempty"`
}

// ingestJob is one forwarded message's run, kept so the verdict can be
// collected after a 202 and returned again to a repeated POST.
type ingestJob struct {
	done     chan struct{}
	response ingestResponse // written before done closes
	finished time.Time      // zero while queued or running; guarded by ingestJobsMu
}

var (
	ingestJobsMu  sync.Mutex
	ingestJobs    = map[string]*ingestJob{}
	ingestPending int // jobs not yet finished
)

func handleIngestPost(e *core.RequestEvent) error {
	if isDraining() {
		e.Response.Header().Set("Retry-After", "60")
		return e.Error(http.StatusServiceUnavailable, "goyangi is restarting; retry in a minute.", nil)
	}

	var req ingestRequest
	dec := json.NewDecoder(e.Request.Body)
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, apis.ErrRequestEntityTooLarge) {
			return apis.ErrRequestEntityTooLarge
		}
		return e.BadRequestError("Body must be JSON: {message, roles, bot_user_id} ("+err.Error()+").", nil)
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return e.BadRequestError("Body must be a single JSON object.", nil)
	}
	in, warnings, err := req.inbound()
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}
	id := in.msg.ID.String()

	// A dry run changes nothing — not even which bots goyangi knows — so it
	// needs no place in the channel's line and no job to collect later: it
	// answers here.
	if in.dryRun {
		resp := responseFor(id, ingestInbound(in))
		resp.DryRun, resp.Reaction, resp.Warnings = true, "", warnings
		return e.JSON(http.StatusOK, resp)
	}

	rememberForwarder(in.door)

	// Queued now, while the request is in hand, so the channel's line follows
	// the order requests arrived in rather than goroutine scheduling.
	channel := in.msg.ChannelID
	job, ok, behind := startJob(id, func() *channelTurn { return queueTurn(channel) }, func() ingestResponse {
		resp := responseFor(id, ingestInbound(in))
		resp.Warnings = warnings
		return resp
	})
	wait := ingestWait
	if behind {
		wait = queuedWait
	}
	return answerJob(e, id, job, ok, wait)
}

// answerJob waits up to `wait` for a job's answer: 200 with it, 202 while it
// runs on, 503 when there was no room to start it.
func answerJob(e *core.RequestEvent, id string, job *ingestJob, ok bool, wait time.Duration) error {
	if !ok {
		e.Response.Header().Set("Retry-After", "30")
		return e.Error(http.StatusServiceUnavailable, "goyangi is busy; retry shortly.", nil)
	}
	select {
	case <-job.done:
		return e.JSON(http.StatusOK, job.response)
	case <-time.After(wait):
		return e.JSON(http.StatusAccepted, processingResponse(id, true))
	case <-e.Request.Context().Done():
		// The caller hung up, or the server is shutting down. Answer as if the
		// wait had run out — never an empty 200 — and let the job carry on.
		return e.JSON(http.StatusAccepted, processingResponse(id, true))
	}
}

// jobStatusHandler answers GET …/{id} for the jobs under a key prefix.
func jobStatusHandler(prefix string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		id := e.Request.PathValue("id")
		ingestJobsMu.Lock()
		sweepIngestJobsLocked(time.Now())
		job, ok := ingestJobs[prefix+id]
		finished := ok && !job.finished.IsZero()
		ingestJobsMu.Unlock()
		switch {
		case !ok:
			return e.NotFoundError("No recent ingest for that message.", nil)
		case !finished:
			return e.JSON(http.StatusAccepted, processingResponse(id, false))
		default:
			return e.JSON(http.StatusOK, job.response)
		}
	}
}

func processingResponse(id string, hint bool) ingestResponse {
	resp := ingestResponse{MessageID: id, Status: "processing"}
	if hint {
		resp.Reason = "still_running"
	}
	return resp
}

// sweepIngestJobsLocked drops verdicts finished more than ingestJobTTL ago.
func sweepIngestJobsLocked(now time.Time) {
	for k, j := range ingestJobs {
		if !j.finished.IsZero() && now.Sub(j.finished) > ingestJobTTL {
			delete(ingestJobs, k)
		}
	}
}

// retryableLocked reports whether a repeated POST should run a finished job
// again: when nothing was decided that a second look could change — an
// `ignored` (an imgur album that didn't resolve last time may now), a `denied`
// (permissions may have been fixed since) — or when nothing was saved, so the
// claim was given back.
func (j *ingestJob) retryableLocked() bool {
	if j.finished.IsZero() {
		return false
	}
	switch ingestStatus(j.response.Status) {
	case statusIgnored, statusDenied:
		return true
	case statusFailed:
		return j.response.Created == 0
	}
	return false
}

// startJob runs a job in the background under key, or returns the job already
// running (or finished) under it. queue, when set, takes the message's place in
// its channel's line; it runs while the registry is locked, so places follow the
// order requests arrived in. ok is false when too much is already pending;
// behind is true when the new job is waiting for others in its channel.
func startJob(key string, queue func() *channelTurn, run func() ingestResponse) (job *ingestJob, ok, behind bool) {
	ingestJobsMu.Lock()
	sweepIngestJobsLocked(time.Now())
	if existing, found := ingestJobs[key]; found && !existing.retryableLocked() {
		ingestJobsMu.Unlock()
		return existing, true, false
	}
	if ingestPending >= maxPendingIngest {
		ingestJobsMu.Unlock()
		return nil, false, false
	}
	job = &ingestJob{done: make(chan struct{})}
	ingestJobs[key] = job
	ingestPending++
	var turn *channelTurn
	if queue != nil {
		turn = queue()
		behind = turn.prev != nil
	}
	ingestJobsMu.Unlock()

	// Counted in Busy(), so a supervised restart waits for it like it waits for
	// a gateway handler.
	handlersInFlight.Add(1)
	go func() {
		defer handlersInFlight.Add(-1)
		defer func() {
			ingestJobsMu.Lock()
			job.finished = time.Now()
			ingestPending--
			ingestJobsMu.Unlock()
			close(job.done)
		}()
		if turn != nil {
			defer turn.release()
		}
		id := strings.TrimPrefix(key, manualJobPrefix)
		job.response = ingestResponse{MessageID: id, Status: string(statusFailed), Reason: reasonInternal}
		defer recoverHandler("ingestAPI")

		if turn != nil {
			turn.wait()
		}
		job.response = run()
	}()
	return job, true, behind
}

func responseFor(messageID string, r ingestResult) ingestResponse {
	resp := ingestResponse{
		MessageID:  messageID,
		Status:     string(r.status),
		Reason:     r.reason,
		Created:    r.outcome.created,
		Total:      r.outcome.total,
		Failed:     r.outcome.failedItems,
		Unresolved: r.outcome.missing,
		Preview:    r.preview,
	}
	switch r.status {
	case statusIngested:
		resp.Reaction = "success"
	case statusSkipped:
		resp.Reaction = "skipped"
	}
	if r.setID != "" {
		resp.Set = publicBaseURL + "/set/" + r.setID
	}
	return resp
}

// forwardedMessage decodes and checks a forwarded message object: present,
// a server message, and within Discord's own limits.
func forwardedMessage(raw json.RawMessage) (discord.Message, error) {
	if len(raw) == 0 {
		return discord.Message{}, fmt.Errorf("`message` is required: the Discord message object as delivered")
	}
	msg, err := parseDiscordMessage(raw)
	if err != nil {
		return msg, err
	}
	switch {
	case msg.ID == 0:
		return msg, fmt.Errorf("`message.id` is required")
	case msg.ChannelID == 0:
		return msg, fmt.Errorf("`message.channel_id` is required")
	case msg.GuildID == nil || *msg.GuildID == 0:
		return msg, fmt.Errorf("`message.guild_id` is required: only server messages are ingested (a message fetched over REST, or one from an interaction's resolved data, lacks it; add it)")
	case msg.Author.ID == 0:
		return msg, fmt.Errorf("`message.author` is required")
	case len(msg.Attachments) > maxMessageAttachments:
		return msg, fmt.Errorf("`message.attachments` has more than %d entries", maxMessageAttachments)
	case utf8.RuneCountInString(msg.Content) > maxMessageContent:
		return msg, fmt.Errorf("`message.content` is longer than %d characters", maxMessageContent)
	case len(msg.Embeds) > maxMessageEmbeds:
		return msg, fmt.Errorf("`message.embeds` has more than %d entries", maxMessageEmbeds)
	}
	for _, a := range msg.Attachments {
		u, perr := url.Parse(a.URL)
		if perr != nil || u.Scheme != "https" || !discordAttachmentHosts[strings.ToLower(u.Hostname())] {
			return msg, fmt.Errorf("attachment %q isn't on Discord's CDN", a.Filename)
		}
	}
	return msg, nil
}

// forwarderID reads the required bot_user_id, which must be a configured
// forwarding bot when INGEST_BOT_USER_IDS is set.
func forwarderID(raw string) (snowflake.ID, error) {
	id, err := snowflake.Parse(strings.TrimSpace(raw))
	if err != nil || id == 0 {
		return 0, fmt.Errorf("`bot_user_id` is required: your bot's user id")
	}
	if len(ingestBotIDs) > 0 && !ingestBotIDs[id] {
		return 0, fmt.Errorf("`bot_user_id` %s isn't the bot goyangi has on file for this token", id)
	}
	return id, nil
}

// postedByForwarder reports whether the forwarding bot posted a message itself:
// as its own user, as an interaction response or follow-up (application_id),
// or through a webhook — anonymous-post features usually post through one, so
// the post can carry a name of its own.
func postedByForwarder(msg discord.Message, door snowflake.ID) bool {
	return msg.Author.ID == door ||
		(msg.ApplicationID != nil && *msg.ApplicationID == door) ||
		msg.WebhookID != nil
}

// inbound validates the request and turns it into what the triggers take.
// warnings are problems that don't stop the message.
func (req ingestRequest) inbound() (in inboundMessage, warnings []string, err error) {
	msg, err := forwardedMessage(req.Message)
	if err != nil {
		return in, nil, err
	}
	door, err := forwarderID(req.BotUserID)
	if err != nil {
		return in, nil, err
	}

	var relay *relayCredit
	if r := req.Relay; r != nil {
		switch {
		case !postedByForwarder(msg, door):
			return in, nil, fmt.Errorf("`relay` is only for messages your bot posted: as itself (author.id = bot_user_id), as an interaction response, or through a webhook")
		case r.Anonymous && r.User != nil:
			return in, nil, fmt.Errorf("`relay` is either anonymous or names a user, not both")
		case r.Anonymous:
			relay = &relayCredit{anonymous: true}
		case r.User == nil || strings.TrimSpace(r.User.Username) == "":
			return in, nil, fmt.Errorf("`relay.user` needs the poster's `id` and `username` (or set `relay.anonymous`)")
		default:
			uid, perr := snowflake.Parse(strings.TrimSpace(r.User.ID))
			if perr != nil || uid == 0 {
				return in, nil, fmt.Errorf("`relay.user.id` isn't a Discord user id")
			}
			relay = &relayCredit{userID: uid.String(), username: strings.TrimSpace(r.User.Username)}
		}
	}

	names := make(map[snowflake.ID]string, len(req.Roles))
	for raw, name := range req.Roles {
		id, perr := snowflake.Parse(raw)
		if perr != nil {
			return in, nil, fmt.Errorf("`roles` keys must be role ids, got %q", raw)
		}
		names[id] = strings.TrimSpace(name)
	}
	// A pinged role without a name is left out, exactly as our own bot leaves
	// out a role it can't look up; the message goes ahead on what remains. The
	// warning is how the forwarding side finds out its role cache missed.
	for _, id := range msg.MentionRoles {
		if names[id] == "" {
			warnings = append(warnings, fmt.Sprintf("no name for pinged role %s; it was ignored", id))
		}
	}

	return inboundMessage{
		msg:     msg,
		guildID: *msg.GuildID,
		door:    door,
		relay:   relay,
		dryRun:  req.DryRun,
		roleNames: func(ids []snowflake.ID) ([]string, error) {
			out := make([]string, 0, len(ids))
			for _, id := range ids {
				if names[id] != "" {
					out = append(out, names[id])
				}
			}
			return out, nil
		},
	}, warnings, nil
}

// parseDiscordMessage decodes a message object. disgo panics on some malformed
// component layouts rather than returning an error; that is the sender's
// mistake, so it comes back as one.
func parseDiscordMessage(raw json.RawMessage) (msg discord.Message, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("`message` isn't a Discord message object")
		}
	}()
	if err := json.Unmarshal(raw, &msg); err != nil {
		return msg, fmt.Errorf("`message` isn't a Discord message object: %v", err)
	}
	return msg, nil
}
