package bot

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/pocketbase/dbx"
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
//	GET  /api/ingest/discord/messages/{id}   the verdict on one, if still pending
//
// Both need `Authorization: Bearer <INGEST_API_TOKEN>`. Without the variable set
// the routes don't exist.
//
// A message can take a while: every item is downloaded before its record is
// created, and the API sits behind Cloudflare, which drops a request after 100
// seconds. So the POST waits up to ingestWait for the verdict and otherwise
// answers 202 "processing", with the GET to collect the result later. The work
// itself is never tied to the request — a dropped connection doesn't cancel it.

// ingestWait is how long a POST holds out for the verdict before answering 202.
// Well inside Cloudflare's 100-second limit. A var for the tests.
var ingestWait = 25 * time.Second

// ingestJobTTL is how long a verdict stays collectable, and how long a retried
// POST gets the original verdict back rather than "duplicate".
const ingestJobTTL = time.Hour

// maxIngestBody bounds the request. A Discord message object is a few KB even
// with embeds; nothing legitimate comes near this.
const maxIngestBody = 1 << 20

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
// Independent of DISCORD_BOT_ENABLED: both doors can be open at once, and
// claimMessage keeps a message seen by both from being ingested twice.
func RegisterIngestAPI(app core.App) {
	tokens := ingestTokens()
	if len(tokens) == 0 {
		return
	}
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		registerIngestRoutes(e, tokens)
		return e.Next()
	})
	slog.Info("ingest API enabled", "tokens", len(tokens))
}

func registerIngestRoutes(e *core.ServeEvent, tokens []string) {
	g := e.Router.Group("/api/ingest/discord")
	g.BindFunc(requireIngestToken(tokens))
	g.POST("/messages", handleIngestPost).Bind(apis.BodyLimit(maxIngestBody))
	g.GET("/messages/{id}", handleIngestStatus)
}

// requireIngestToken checks the bearer token in constant time. A route-level
// check rather than a PocketBase auth record: the caller is a bot, not a user,
// and the token grants exactly this and nothing else.
func requireIngestToken(tokens []string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		got, ok := strings.CutPrefix(e.Request.Header.Get("Authorization"), "Bearer ")
		if ok {
			for _, t := range tokens {
				if subtle.ConstantTimeCompare([]byte(got), []byte(t)) == 1 {
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
	// The forwarding bot's own user id. @-mentioning it is the explicit
	// trigger, and its own messages are never ingested. Optional: without it
	// there is no @-mention trigger.
	BotUserID string `json:"bot_user_id"`
}

// ingestResponse is the verdict, as JSON.
type ingestResponse struct {
	MessageID string `json:"message_id"`
	// ignored | duplicate | skipped | ingested | failed | processing
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// The marker to leave on the message: "success", "skipped" or "" for none.
	Reaction   string   `json:"reaction"`
	Created    int      `json:"created"`
	Total      int      `json:"total"`
	Failed     []string `json:"failed,omitempty"`
	Unresolved []string `json:"unresolved,omitempty"`
	Set        string   `json:"set,omitempty"`
}

// ingestJob is one forwarded message's run, kept so the verdict can be
// collected after a 202 and returned again to a retry.
type ingestJob struct {
	done     chan struct{}
	response ingestResponse
	started  time.Time
}

var (
	ingestJobsMu sync.Mutex
	ingestJobs   = map[string]*ingestJob{}
)

func handleIngestPost(e *core.RequestEvent) error {
	var req ingestRequest
	if err := json.NewDecoder(e.Request.Body).Decode(&req); err != nil {
		return e.BadRequestError("Body must be JSON: {message, roles, bot_user_id}.", err)
	}
	in, err := req.inbound()
	if err != nil {
		return e.BadRequestError(err.Error(), nil)
	}

	job, fresh := startIngestJob(in)
	if fresh {
		slog.Info("ingest API: message received", "message", in.msg.ID, "channel", in.msg.ChannelID)
	}

	select {
	case <-job.done:
		return e.JSON(http.StatusOK, job.response)
	case <-time.After(ingestWait):
		return e.JSON(http.StatusAccepted, ingestResponse{
			MessageID: in.msg.ID.String(),
			Status:    "processing",
			Reason:    "still downloading; collect the verdict from GET /api/ingest/discord/messages/" + in.msg.ID.String(),
		})
	case <-e.Request.Context().Done():
		// The caller gave up; the job carries on regardless.
		return nil
	}
}

func handleIngestStatus(e *core.RequestEvent) error {
	id := e.Request.PathValue("id")
	ingestJobsMu.Lock()
	job, ok := ingestJobs[id]
	ingestJobsMu.Unlock()
	if !ok {
		return e.NotFoundError("No recent ingest for that message.", nil)
	}
	select {
	case <-job.done:
		return e.JSON(http.StatusOK, job.response)
	default:
		return e.JSON(http.StatusAccepted, ingestResponse{MessageID: id, Status: "processing"})
	}
}

// startIngestJob runs the message through ingestInbound in the background, or
// returns the job already running (or finished) for it. fresh is false for a
// retry.
func startIngestJob(in inboundMessage) (job *ingestJob, fresh bool) {
	id := in.msg.ID.String()

	ingestJobsMu.Lock()
	now := time.Now()
	for k, j := range ingestJobs {
		select {
		case <-j.done:
			if now.Sub(j.started) > ingestJobTTL {
				delete(ingestJobs, k)
			}
		default:
		}
	}
	if existing, ok := ingestJobs[id]; ok {
		ingestJobsMu.Unlock()
		return existing, false
	}
	job = &ingestJob{done: make(chan struct{}), started: now}
	ingestJobs[id] = job
	ingestJobsMu.Unlock()

	// Counted in Busy(), so a supervised restart waits for it like it waits for
	// a gateway handler.
	handlersInFlight.Add(1)
	go func() {
		defer handlersInFlight.Add(-1)
		defer close(job.done)
		job.response = ingestResponse{MessageID: id, Status: string(statusFailed), Reason: "internal error"}
		defer recoverHandler("ingestAPI")
		job.response = responseFor(id, ingestInbound(in))
	}()
	return job, true
}

func responseFor(messageID string, r ingestResult) ingestResponse {
	resp := ingestResponse{
		MessageID:  messageID,
		Status:     string(r.status),
		Reason:     r.reason,
		Created:    r.outcome.created,
		Total:      r.outcome.total,
		Failed:     r.outcome.failures,
		Unresolved: r.outcome.missing,
	}
	if resp.Reason == "" {
		resp.Reason = r.outcome.fatal
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

// inbound validates the request and turns it into what the triggers take.
func (req ingestRequest) inbound() (inboundMessage, error) {
	if len(req.Message) == 0 {
		return inboundMessage{}, fmt.Errorf("`message` is required: the Discord message object as delivered")
	}
	var msg discord.Message
	if err := json.Unmarshal(req.Message, &msg); err != nil {
		return inboundMessage{}, fmt.Errorf("`message` isn't a Discord message object: %v", err)
	}
	switch {
	case msg.ID == 0:
		return inboundMessage{}, fmt.Errorf("`message.id` is required")
	case msg.ChannelID == 0:
		return inboundMessage{}, fmt.Errorf("`message.channel_id` is required")
	case msg.GuildID == nil || *msg.GuildID == 0:
		return inboundMessage{}, fmt.Errorf("`message.guild_id` is required: only server messages are ingested")
	case msg.Author.ID == 0:
		return inboundMessage{}, fmt.Errorf("`message.author` is required")
	}

	var selfID snowflake.ID
	if req.BotUserID != "" {
		id, err := snowflake.Parse(req.BotUserID)
		if err != nil {
			return inboundMessage{}, fmt.Errorf("`bot_user_id` isn't a Discord id")
		}
		selfID = id
	}

	// Every pinged role needs its name: a role ping is a trigger, and the name
	// is the metadata. Refused up front rather than ingested half-attributed.
	names := make(map[snowflake.ID]string, len(req.Roles))
	for raw, name := range req.Roles {
		id, err := snowflake.Parse(raw)
		if err != nil {
			return inboundMessage{}, fmt.Errorf("`roles` keys must be role ids, got %q", raw)
		}
		names[id] = name
	}
	for _, id := range msg.MentionRoles {
		if strings.TrimSpace(names[id]) == "" {
			return inboundMessage{}, fmt.Errorf("`roles` has no name for pinged role %s", id)
		}
	}

	return inboundMessage{
		msg:     msg,
		guildID: *msg.GuildID,
		selfID:  selfID,
		roleNames: func(ids []snowflake.ID) ([]string, error) {
			out := make([]string, 0, len(ids))
			for _, id := range ids {
				out = append(out, names[id])
			}
			return out, nil
		},
	}, nil
}

// alreadyIngested reports whether the archive already holds posts from this
// message. The in-memory claim only covers this process's lifetime; this covers
// a retry after a restart, and a message our own bot took before the switch.
func alreadyIngested(jumpLink string) bool {
	rec, err := App.FindFirstRecordByFilter("contents", "discord = {:link}", dbx.Params{"link": jumpLink})
	return err == nil && rec != nil
}
