package bot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

const (
	testToken     = "test-ingest-token"
	testGuild     = "1000000000000000001"
	testChannel   = "1000000000000000002"
	testBotUser   = "1000000000000000003"
	testRole      = "1000000000000000004"
	testRole2     = "1000000000000000005"
	testOurBot    = "1000000000000000006"
	testAuthor    = "1000000000000000099"
	testRelayApp  = "1000000000000000077"
	testCDNPrefix = "https://cdn.discordapp.com/attachments/1/2/"
)

// A 1×1 PNG, so a download yields a real image.
var testPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0,
	0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x89, 0x99, 0x3d, 0x1d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

type ingestFixture struct {
	app *tests.TestApp
	mux http.Handler
	// release unblocks downloads of files named slow*.png.
	release     chan struct{}
	releaseOnce sync.Once
	downloads   atomic.Int64
}

func (f *ingestFixture) unblock() { f.releaseOnce.Do(func() { close(f.release) }) }

// toMediaServer sends every download to the local media server, so attachment
// URLs can be the real Discord CDN form the API insists on.
type toMediaServer struct {
	base *url.URL
	rt   http.RoundTripper
}

func (t toMediaServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.URL.Scheme, r2.URL.Host, r2.Host = t.base.Scheme, t.base.Host, t.base.Host
	return t.rt.RoundTrip(r2)
}

// newIngestFixture is a test app with the real schema, IVE and Yujin in the
// directory, a media server behind the CDN hostnames, the test channel
// allowlisted, and the ingest routes mounted with testToken.
func newIngestFixture(t *testing.T) *ingestFixture {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("test app: %v", err)
	}
	t.Cleanup(app.Cleanup)
	schema, err := os.ReadFile("../pb_schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if err := app.ImportCollectionsByMarshaledJSON(schema, false); err != nil {
		t.Fatalf("import schema: %v", err)
	}

	groups, _ := app.FindCollectionByNameOrId("groups")
	g := core.NewRecord(groups)
	g.Set("name", "IVE")
	g.Set("code", "ive")
	mustSaveRecord(t, app, g)
	idols, _ := app.FindCollectionByNameOrId("groups_idols")
	i := core.NewRecord(idols)
	i.Set("name", "Yujin")
	i.Set("code", "yujin")
	i.Set("group", g.Id)
	mustSaveRecord(t, app, i)

	// Package state from other tests: the app, the directory cache, claims,
	// sets, chains, jobs, known bots, and the limits tests override.
	Init(app)
	directoryCache = nil
	processedMu.Lock()
	processedMsgs = map[string]time.Time{}
	processedMu.Unlock()
	setRegistryMu.Lock()
	setRegistry = map[string]setEntry{}
	setRegistryMu.Unlock()
	chainMu.Lock()
	chains = map[string]chainEntry{}
	chainMu.Unlock()
	ingestJobsMu.Lock()
	ingestJobs, ingestPending = map[string]*ingestJob{}, 0
	ingestJobsMu.Unlock()
	forwardersMu.Lock()
	forwarders = map[snowflake.ID]bool{}
	forwardersMu.Unlock()
	ourBotID.Store(0)
	realWait, realQueued, realPending, realDraining, realBots := ingestWait, queuedWait, maxPendingIngest, isDraining, ingestBotIDs
	realGuilds, realRoles := manualGuildIDs, manualIngestRoleIDs
	ingestBotIDs = map[snowflake.ID]bool{}
	manualGuildIDs = map[snowflake.ID]bool{snowflake.MustParse(testGuild): true}
	manualIngestRoleIDs = map[snowflake.ID]bool{snowflake.MustParse(testModRole): true}
	t.Cleanup(func() {
		ingestWait, queuedWait, maxPendingIngest, isDraining, ingestBotIDs = realWait, realQueued, realPending, realDraining, realBots
		manualGuildIDs, manualIngestRoleIDs = realGuilds, realRoles
	})

	channel, _ := snowflake.Parse(testChannel)
	allowedChannelIDs[channel] = true
	t.Cleanup(func() { delete(allowedChannelIDs, channel) })

	f := &ingestFixture{app: app, release: make(chan struct{})}
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.downloads.Add(1)
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if strings.HasPrefix(name, "slow") {
			<-f.release
		}
		if strings.HasPrefix(name, "missing") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(testPNG)
	}))
	// Cleanups run last-in first-out: unblock slow downloads before the server
	// waits for them to finish.
	t.Cleanup(media.Close)
	t.Cleanup(f.unblock)
	base, _ := url.Parse(media.URL)
	realClient := mediaDownloadClient
	mediaDownloadClient = &http.Client{Transport: toMediaServer{base: base, rt: http.DefaultTransport}}
	t.Cleanup(func() { mediaDownloadClient = realClient })

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	serve := &core.ServeEvent{App: app, Router: router}
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		registerIngestRoutes(e, []string{testToken})
		return e.Next()
	})
	if err := app.OnServe().Trigger(serve, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		f.mux = mux
		return err
	}); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return f
}

func mustSaveRecord(t *testing.T, app core.App, r *core.Record) {
	t.Helper()
	if err := app.SaveNoValidate(r); err != nil {
		t.Fatalf("save %s: %v", r.Collection().Name, err)
	}
}

// message is the test's description of a Discord message; body renders it the
// way the gateway delivers one.
type message struct {
	id, channel, author, username, content string
	bot                                    bool
	mentionRoles                           []string
	files                                  []string // attachment filenames
	replyTo                                string   // the message this one replies to
}

// request is the rest of an ingest request around a message.
type request struct {
	roles  map[string]string
	botID  string // defaults to testBotUser; "-" sends none
	relay  any
	dryRun bool
}

func body(m message, r request) string {
	if m.channel == "" {
		m.channel = testChannel
	}
	if m.author == "" {
		m.author = testAuthor
	}
	if m.username == "" {
		m.username = "nabi"
	}
	attachments := []any{}
	for i, name := range m.files {
		attachments = append(attachments, map[string]any{
			"id":           fmt.Sprintf("10000000000000005%02d", i),
			"filename":     name,
			"content_type": "image/png",
			"size":         len(testPNG),
			"url":          testCDNPrefix + name,
			"proxy_url":    testCDNPrefix + name,
		})
	}
	msg := map[string]any{
		"id":            m.id,
		"channel_id":    m.channel,
		"guild_id":      testGuild,
		"author":        map[string]any{"id": m.author, "username": m.username, "bot": m.bot},
		"content":       m.content,
		"mention_roles": append([]string{}, m.mentionRoles...),
		"attachments":   attachments,
		"embeds":        []any{},
		"timestamp":     "2026-10-11T12:00:00.000000+00:00",
		"type":          0,
	}
	if m.replyTo != "" {
		msg["type"] = 19
		msg["message_reference"] = map[string]any{"type": 0, "message_id": m.replyTo, "channel_id": m.channel, "guild_id": testGuild}
	}
	req := map[string]any{"message": msg, "roles": r.roles}
	switch r.botID {
	case "":
		req["bot_user_id"] = testBotUser
	case "-":
	default:
		req["bot_user_id"] = r.botID
	}
	if r.relay != nil {
		req["relay"] = r.relay
	}
	if r.dryRun {
		req["dry_run"] = true
	}
	out, _ := json.Marshal(req)
	return string(out)
}

func (f *ingestFixture) do(t *testing.T, method, path, body, auth string) (int, ingestResponse, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	var resp ingestResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp, rec.Body.String()
}

func (f *ingestFixture) post(t *testing.T, body string) (int, ingestResponse, string) {
	t.Helper()
	return f.do(t, http.MethodPost, "/api/ingest/discord/messages", body, "Bearer "+testToken)
}

func (f *ingestFixture) get(t *testing.T, id string) (int, ingestResponse, string) {
	t.Helper()
	return f.do(t, http.MethodGet, "/api/ingest/discord/messages/"+id, "", "Bearer "+testToken)
}

// await polls for a verdict.
func (f *ingestFixture) await(t *testing.T, id string) ingestResponse {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, resp, raw := f.get(t, id)
		if code == http.StatusOK {
			return resp
		}
		if time.Now().After(deadline) {
			t.Fatalf("no verdict for %s: %d %s", id, code, raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *ingestFixture) count(t *testing.T, collection string) int {
	t.Helper()
	n, err := f.app.CountRecords(collection)
	if err != nil {
		t.Fatalf("count %s: %v", collection, err)
	}
	return int(n)
}

func (f *ingestFixture) postFor(t *testing.T, messageID string) *core.Record {
	t.Helper()
	link := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", testGuild, testChannel, messageID)
	rec, err := f.app.FindFirstRecordByFilter("contents", "discord = {:link}", dbx.Params{"link": link})
	if err != nil {
		t.Fatalf("no post for message %s: %v", messageID, err)
	}
	return rec
}

func (f *ingestFixture) uploaderOf(t *testing.T, post *core.Record) string {
	t.Helper()
	up, err := f.app.FindRecordById("uploaders", post.GetString("uploader"))
	if err != nil {
		t.Fatalf("uploader of %s: %v", post.Id, err)
	}
	return up.GetString("name")
}

var yujin = map[string]string{testRole: "Yujin [IVE]"}

func expect(t *testing.T, name string, code int, resp ingestResponse, raw string, wantCode int, wantStatus, wantReason, wantReaction string) {
	t.Helper()
	if code != wantCode || resp.Status != wantStatus || resp.Reason != wantReason || resp.Reaction != wantReaction {
		t.Errorf("%s: got %d %q/%q/%q, want %d %q/%q/%q — %s", name, code, resp.Status, resp.Reason, resp.Reaction,
			wantCode, wantStatus, wantReason, wantReaction, raw)
	}
}

func TestIngestAPIAuth(t *testing.T) {
	f := newIngestFixture(t)
	b := body(message{id: "2000000000000000001"}, request{})
	for name, auth := range map[string]string{"no token": "", "wrong token": "Bearer nope", "no scheme": testToken} {
		if code, _, raw := f.do(t, http.MethodPost, "/api/ingest/discord/messages", b, auth); code != http.StatusUnauthorized {
			t.Errorf("%s: %d, want 401 — %s", name, code, raw)
		}
	}
	if code, _, raw := f.do(t, http.MethodGet, "/api/ingest/discord/messages/1", "", ""); code != http.StatusUnauthorized {
		t.Errorf("status lookup without token: %d, want 401 — %s", code, raw)
	}
	// The scheme is case-insensitive, as in PocketBase's own auth.
	if code, _, raw := f.do(t, http.MethodPost, "/api/ingest/discord/messages", b, "bearer "+testToken); code != http.StatusOK {
		t.Errorf("lowercase scheme: %d, want 200 — %s", code, raw)
	}
}

func TestIngestAPIRejectsMalformedRequests(t *testing.T) {
	f := newIngestFixture(t)
	eleven := make([]string, 11)
	for i := range eleven {
		eleven[i] = fmt.Sprintf("f%d.png", i)
	}
	elsewhere := strings.ReplaceAll(body(message{id: "2000000000000000006", mentionRoles: []string{testRole}, files: []string{"y.png"}}, request{roles: yujin}),
		"https://cdn.discordapp.com/", "https://example.com/")
	cases := map[string]string{
		"not json":                     "{",
		"two objects":                  `{"message":{}} {}`,
		"no message":                   `{"roles":{}, "bot_user_id":"` + testBotUser + `"}`,
		"no guild":                     `{"bot_user_id":"` + testBotUser + `","message":{"id":"2000000000000000002","channel_id":"` + testChannel + `","author":{"id":"5","username":"x"}}}`,
		"numeric id":                   `{"bot_user_id":"` + testBotUser + `","message":{"id":2000000000000000002,"channel_id":"` + testChannel + `","guild_id":"` + testGuild + `","author":{"id":"5","username":"x"}}}`,
		"no bot_user_id":               body(message{id: "2000000000000000003"}, request{botID: "-"}),
		"11 attachments":               body(message{id: "2000000000000000004", files: eleven}, request{}),
		"content too long":             body(message{id: "2000000000000000005", content: strings.Repeat("a", 4001)}, request{}),
		"attachment off Discord's CDN": elsewhere,
		"relay on someone else's post": body(message{id: "2000000000000000007", files: []string{"y.png"}},
			request{relay: map[string]any{"anonymous": true}}),
		"relay without a user": body(message{id: "2000000000000000008", author: testBotUser, files: []string{"y.png"}},
			request{relay: map[string]any{}}),
		"relay both anonymous and named": body(message{id: "2000000000000000009", author: testBotUser, files: []string{"y.png"}},
			request{relay: map[string]any{"anonymous": true, "user": map[string]any{"id": "5", "username": "x"}}}),
		// disgo panics on a top-level button; that's the sender's mistake.
		"malformed components": `{"bot_user_id":"` + testBotUser + `","message":{"id":"2000000000000000010","channel_id":"` + testChannel +
			`","guild_id":"` + testGuild + `","author":{"id":"5","username":"x"},"components":[{"type":2,"style":1,"label":"x","custom_id":"x"}]}}`,
	}
	for name, b := range cases {
		if code, _, raw := f.post(t, b); code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400 — %s", name, code, raw)
		}
	}

	huge := `{"message":"` + strings.Repeat("a", maxIngestBody) + `"}`
	if code, _, raw := f.post(t, huge); code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413 — %.200s", code, raw)
	}
}

func TestIngestAPIIgnoresWhatTheBotWouldIgnore(t *testing.T) {
	f := newIngestFixture(t)
	cases := []struct {
		name, reason string
		m            message
	}{
		{"channel outside the allowlist", reasonChannelNotListed,
			message{id: "2000000000000000010", channel: "1000000000000000777", mentionRoles: []string{testRole}, files: []string{"y.png"}}},
		{"no media", reasonNoMedia, message{id: "2000000000000000011", mentionRoles: []string{testRole}, content: "hi"}},
		{"no trigger", reasonNoTrigger, message{id: "2000000000000000012", content: "look", files: []string{"y.png"}}},
		{"the forwarding bot's own post", reasonOwnMessage,
			message{id: "2000000000000000013", author: testBotUser, mentionRoles: []string{testRole}, files: []string{"y.png"}}},
	}
	for _, c := range cases {
		code, resp, raw := f.post(t, body(c.m, request{roles: yujin}))
		expect(t, c.name, code, resp, raw, http.StatusOK, string(statusIgnored), c.reason, "")
	}
	if n := f.count(t, "contents"); n != 0 {
		t.Errorf("%d posts created, want none", n)
	}
}

func TestIngestAPIRolePingIngests(t *testing.T) {
	f := newIngestFixture(t)
	const id = "2000000000000000020"
	b := body(message{id: id, mentionRoles: []string{testRole}, files: []string{"yujin.png"}}, request{roles: yujin})

	code, resp, raw := f.post(t, b)
	expect(t, "first", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if resp.Created != 1 || resp.Total != 1 || !strings.Contains(resp.Set, "/set/") {
		t.Errorf("verdict %+v, want 1/1 and a set link", resp)
	}
	post := f.postFor(t, id)
	if !strings.HasSuffix(resp.Set, "/set/"+post.GetString("set")) {
		t.Errorf("post set %q doesn't match the reported %q", post.GetString("set"), resp.Set)
	}
	// Credited the way our own bot credits: the author's username.
	if got := f.uploaderOf(t, post); got != "nabi" {
		t.Errorf("credited %q, want nabi", got)
	}

	// A repeat gets the original verdict back, and the GET has it too.
	code, again, raw := f.post(t, b)
	expect(t, "repeat", code, again, raw, http.StatusOK, string(statusIngested), "", "success")
	if code, got, raw := f.get(t, id); code != http.StatusOK || got.Status != string(statusIngested) {
		t.Errorf("status lookup: %d %q — %s", code, got.Status, raw)
	}

	// After a restart (no claims, no jobs) the archive itself answers.
	processedMu.Lock()
	processedMsgs = map[string]time.Time{}
	processedMu.Unlock()
	ingestJobsMu.Lock()
	ingestJobs = map[string]*ingestJob{}
	ingestJobsMu.Unlock()
	code, dup, raw := f.post(t, b)
	expect(t, "after restart", code, dup, raw, http.StatusOK, string(statusDuplicate), reasonAlreadyArchived, "")
	if n := f.count(t, "contents"); n != 1 {
		t.Errorf("%d posts, want exactly 1", n)
	}
}

func TestIngestAPIUnnamedRoleIsAWarning(t *testing.T) {
	f := newIngestFixture(t)
	code, resp, raw := f.post(t, body(message{id: "2000000000000000021", mentionRoles: []string{testRole, testRole2}, files: []string{"y.png"}},
		request{roles: map[string]string{testRole: "Yujin [IVE]", testRole2: ""}}))
	expect(t, "unnamed second role", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], testRole2) {
		t.Errorf("warnings %q, want one naming %s", resp.Warnings, testRole2)
	}
}

func TestIngestAPISkipsLikeTheBot(t *testing.T) {
	f := newIngestFixture(t)

	// A role ping plus an @-mention of goyangi's bot is a re-announcement —
	// whichever bot is mentioned. Our own bot counts too, which is what keeps
	// a message both doors see from being skipped by one and taken by the other.
	ourBotID.Store(uint64(snowflake.MustParse(testOurBot)))
	for i, mention := range []string{testBotUser, testOurBot} {
		code, resp, raw := f.post(t, body(message{id: fmt.Sprintf("200000000000000003%d", i), content: "<@" + mention + "> look",
			mentionRoles: []string{testRole}, files: []string{"y.png"}}, request{roles: yujin}))
		expect(t, "ping + mention of "+mention, code, resp, raw, http.StatusOK, string(statusSkipped), reasonReannouncement, "skipped")
	}

	// An uploader who turned off automatic import.
	uploaders, _ := f.app.FindCollectionByNameOrId("uploaders")
	u := core.NewRecord(uploaders)
	u.Set("name", "optedout")
	u.Set("skipDiscordImport", true)
	mustSaveRecord(t, f.app, u)
	code, resp, raw := f.post(t, body(message{id: "2000000000000000035", author: "1000000000000000098", username: "optedout",
		mentionRoles: []string{testRole}, files: []string{"y.png"}}, request{roles: yujin}))
	expect(t, "opted out", code, resp, raw, http.StatusOK, string(statusSkipped), reasonOptedOut, "skipped")

	if n := f.count(t, "contents"); n != 0 {
		t.Errorf("%d posts created, want none", n)
	}
}

// The gateway handles a channel's messages one at a time; over the API they
// arrive concurrently. A follow-up and a repeat ping must still land in the set
// the first message opened, even while that one is still downloading.
func TestIngestAPIKeepsAChannelInOrder(t *testing.T) {
	f := newIngestFixture(t)
	ingestWait, queuedWait = 20*time.Millisecond, 20*time.Millisecond

	ids := []string{"2000000000000000040", "2000000000000000041", "2000000000000000042"}
	bodies := []string{
		body(message{id: ids[0], mentionRoles: []string{testRole}, files: []string{"slow.png"}}, request{roles: yujin}),
		body(message{id: ids[1], files: []string{"b.png"}}, request{}),
		body(message{id: ids[2], mentionRoles: []string{testRole}, files: []string{"c.png"}}, request{roles: yujin}),
	}
	for i, b := range bodies {
		if code, resp, raw := f.post(t, b); code != http.StatusAccepted || resp.Status != "processing" {
			t.Fatalf("message %d: %d %q, want 202 processing while the first downloads — %s", i, code, resp.Status, raw)
		}
	}
	f.unblock()

	for _, id := range ids {
		if v := f.await(t, id); v.Status != string(statusIngested) {
			t.Errorf("message %s: %+v, want ingested", id, v)
		}
	}
	if sets, posts := f.count(t, "contents_sets"), f.count(t, "contents"); sets != 1 || posts != 3 {
		t.Errorf("%d sets and %d posts, want 1 set of 3", sets, posts)
	}
}

// A post the forwarding bot made on someone's behalf is that person's post;
// an anonymous one is nobody's, and never joins another post's set.
func TestIngestAPIRelayedPosts(t *testing.T) {
	f := newIngestFixture(t)
	person := map[string]any{"user": map[string]any{"id": "1000000000000000055", "username": "hyein"}}
	anonymous := map[string]any{"anonymous": true}

	code, resp, raw := f.post(t, body(message{id: "2000000000000000050", author: testBotUser, username: "fimbot", bot: true,
		mentionRoles: []string{testRole}, files: []string{"a.png"}}, request{roles: yujin, relay: person}))
	expect(t, "relayed for a person", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if got := f.uploaderOf(t, f.postFor(t, "2000000000000000050")); got != "hyein" {
		t.Errorf("credited %q, want hyein", got)
	}
	// The same person's bare follow-up through the relay joins their set.
	code, resp, raw = f.post(t, body(message{id: "2000000000000000051", author: testBotUser, username: "fimbot", bot: true,
		files: []string{"b.png"}}, request{relay: person}))
	expect(t, "their follow-up", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if f.postFor(t, "2000000000000000051").GetString("set") != f.postFor(t, "2000000000000000050").GetString("set") {
		t.Error("the relayed follow-up didn't join its poster's set")
	}

	code, resp, raw = f.post(t, body(message{id: "2000000000000000052", author: testBotUser, username: "fimbot", bot: true,
		mentionRoles: []string{testRole}, files: []string{"c.png"}}, request{roles: yujin, relay: anonymous}))
	expect(t, "anonymous", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if got := f.uploaderOf(t, f.postFor(t, "2000000000000000052")); got != "fimbot" {
		t.Errorf("anonymous post credited %q, want the app (fimbot)", got)
	}
	// Another anonymous post right after is not a follow-up of the first: it
	// could be anyone's.
	code, resp, raw = f.post(t, body(message{id: "2000000000000000053", author: testBotUser, username: "fimbot", bot: true,
		files: []string{"d.png"}}, request{relay: anonymous}))
	expect(t, "next anonymous post", code, resp, raw, http.StatusOK, string(statusIgnored), reasonNoTrigger, "")
}

// The same goes for other relay apps, through either door: posts that only
// name the app aren't chained together across people.
func TestRelayAppPostsDontChainAcrossPeople(t *testing.T) {
	f := newIngestFixture(t)
	code, resp, raw := f.post(t, body(message{id: "2000000000000000060", author: testRelayApp, username: "wonheebot", bot: true,
		mentionRoles: []string{testRole}, files: []string{"a.png"}}, request{roles: yujin}))
	expect(t, "relay app post", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	code, resp, raw = f.post(t, body(message{id: "2000000000000000061", author: testRelayApp, username: "wonheebot", bot: true,
		files: []string{"b.png"}}, request{}))
	expect(t, "next post through the app", code, resp, raw, http.StatusOK, string(statusIgnored), reasonNoTrigger, "")
}

// Chaining is off for them, but a reply is the app's own word: a relay app
// replying to its own post continues that set, as per-idol bots post a drop.
// Another app replying to it is somebody else, and doesn't.
func TestRelayAppRepliesContinueItsOwnSet(t *testing.T) {
	f := newIngestFixture(t)
	code, resp, raw := f.post(t, body(message{id: "2000000000000000062", author: testRelayApp, username: "kyujinbot", bot: true,
		mentionRoles: []string{testRole}, files: []string{"a.png"}}, request{roles: yujin}))
	expect(t, "relay app post", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")

	code, resp, raw = f.post(t, body(message{id: "2000000000000000063", author: testRelayApp, username: "kyujinbot", bot: true,
		files: []string{"b.png"}, replyTo: "2000000000000000062"}, request{}))
	expect(t, "its reply to itself", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if f.postFor(t, "2000000000000000063").GetString("set") != f.postFor(t, "2000000000000000062").GetString("set") {
		t.Error("the app's reply didn't join the set its post opened")
	}

	code, resp, raw = f.post(t, body(message{id: "2000000000000000064", author: "1000000000000000078", username: "otherbot", bot: true,
		files: []string{"c.png"}, replyTo: "2000000000000000062"}, request{}))
	expect(t, "another app's reply", code, resp, raw, http.StatusOK, string(statusIgnored), reasonNoTrigger, "")

	// Pinging the same role again in the reply changes nothing: it still joins.
	code, resp, raw = f.post(t, body(message{id: "2000000000000000066", author: testRelayApp, username: "kyujinbot", bot: true,
		mentionRoles: []string{testRole}, files: []string{"d.png"}, replyTo: "2000000000000000062"}, request{roles: yujin}))
	expect(t, "its reply with the ping", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if f.postFor(t, "2000000000000000066").GetString("set") != f.postFor(t, "2000000000000000062").GetString("set") {
		t.Error("the app's reply with a ping didn't join the set its post opened")
	}
	if n := f.count(t, "contents_sets"); n != 1 {
		t.Errorf("%d sets, want 1", n)
	}
}

func TestIngestAPIMentionWithoutLinesTriesTheText(t *testing.T) {
	f := newIngestFixture(t)
	code, resp, raw := f.post(t, body(message{id: "2000000000000000065", content: "<@" + testBotUser + "> Yujin today",
		files: []string{"y.png"}}, request{}))
	expect(t, "mention naming an idol", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
}

func TestIngestAPIDryRun(t *testing.T) {
	f := newIngestFixture(t)
	const id = "2000000000000000070"
	m := message{id: id, mentionRoles: []string{testRole}, files: []string{"yujin.png"}}

	code, resp, raw := f.post(t, body(m, request{roles: yujin, dryRun: true}))
	if code != http.StatusOK || !resp.DryRun || resp.Status != string(statusIngested) || resp.Reaction != "" || resp.Preview == nil {
		t.Fatalf("dry run: %d %+v — %s", code, resp, raw)
	}
	p := resp.Preview
	if p.Trigger != "role_ping" || p.Uploader != "nabi" || len(p.Idols) != 1 || p.Idols[0] != "Yujin" ||
		len(p.Groups) != 1 || p.Groups[0] != "IVE" || len(p.Items) != 1 || p.Items[0].Filename != "yujin.png" {
		t.Errorf("preview %+v", p)
	}
	if n := f.count(t, "contents") + f.count(t, "contents_sets") + f.count(t, "uploaders"); n != 0 || f.downloads.Load() != 0 {
		t.Errorf("a dry run left %d records and made %d downloads", n, f.downloads.Load())
	}

	// It claimed nothing: the real thing still runs.
	code, resp, raw = f.post(t, body(m, request{roles: yujin}))
	expect(t, "real run after a dry run", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
}

func TestIngestAPIFailedRunCanBeRetried(t *testing.T) {
	f := newIngestFixture(t)
	b := body(message{id: "2000000000000000080", mentionRoles: []string{testRole}, files: []string{"missing.png"}}, request{roles: yujin})

	code, resp, raw := f.post(t, b)
	expect(t, "download fails", code, resp, raw, http.StatusOK, string(statusFailed), reasonNothingSaved, "")
	if len(resp.Failed) != 1 || resp.Failed[0] != "missing.png" {
		t.Errorf("failed %q, want just the filename", resp.Failed)
	}
	if n := f.count(t, "contents_sets"); n != 0 {
		t.Errorf("%d sets left behind, want the empty one removed", n)
	}
	before := f.downloads.Load()
	code, resp, raw = f.post(t, b)
	expect(t, "retry", code, resp, raw, http.StatusOK, string(statusFailed), reasonNothingSaved, "")
	if f.downloads.Load() == before {
		t.Error("the retry didn't try again")
	}
}

func TestIngestAPIBackPressure(t *testing.T) {
	f := newIngestFixture(t)

	isDraining = func() bool { return true }
	code, _, raw := f.post(t, body(message{id: "2000000000000000090", mentionRoles: []string{testRole}, files: []string{"y.png"}}, request{roles: yujin}))
	if code != http.StatusServiceUnavailable {
		t.Errorf("while draining: %d, want 503 — %s", code, raw)
	}
	isDraining = func() bool { return false }

	maxPendingIngest = 1
	ingestWait, queuedWait = 20*time.Millisecond, 20*time.Millisecond
	if code, _, raw := f.post(t, body(message{id: "2000000000000000091", mentionRoles: []string{testRole}, files: []string{"slow.png"}}, request{roles: yujin})); code != http.StatusAccepted {
		t.Fatalf("first: %d — %s", code, raw)
	}
	code, _, raw = f.post(t, body(message{id: "2000000000000000092", mentionRoles: []string{testRole}, files: []string{"y.png"}}, request{roles: yujin}))
	if code != http.StatusServiceUnavailable {
		t.Errorf("queue full: %d, want 503 — %s", code, raw)
	}
	f.unblock()
	f.await(t, "2000000000000000091")
}

func TestIngestAPIUnknownStatus(t *testing.T) {
	f := newIngestFixture(t)
	if code, _, raw := f.get(t, "123"); code != http.StatusNotFound {
		t.Errorf("status %d, want 404 — %s", code, raw)
	}
}

// What the gateway door sees: a bot that forwards over the API is one of
// goyangi's own, and its posts are left to it.
func TestGatewayDoorLeavesForwarderPostsToTheAPI(t *testing.T) {
	newIngestFixture(t)
	rememberForwarder(snowflake.MustParse(testBotUser))
	var msg discord.Message
	if err := json.Unmarshal([]byte(`{"id":"2000000000000000095","channel_id":"`+testChannel+`","guild_id":"`+testGuild+
		`","author":{"id":"`+testBotUser+`","username":"fimbot","bot":true},"content":"","mention_roles":["`+testRole+
		`"],"attachments":[{"id":"1","filename":"y.png","url":"`+testCDNPrefix+`y.png"}]}`), &msg); err != nil {
		t.Fatal(err)
	}
	result := ingestInbound(inboundMessage{
		msg: msg, guildID: snowflake.MustParse(testGuild), door: snowflake.MustParse(testOurBot),
		roleNames: func([]snowflake.ID) ([]string, error) { return []string{"Yujin [IVE]"}, nil },
	})
	if result.status != statusIgnored || result.reason != reasonOwnMessage {
		t.Errorf("got %s/%s, want ignored/own_message", result.status, result.reason)
	}
}

func TestIngestTokens(t *testing.T) {
	t.Setenv("INGEST_API_TOKEN", " , ")
	if got := ingestTokens(); len(got) != 0 {
		t.Errorf("tokens %q, want none", got)
	}
	t.Setenv("INGEST_API_TOKEN", "old, new")
	if got := ingestTokens(); len(got) != 2 || got[0] != "old" || got[1] != "new" {
		t.Errorf("tokens %q, want [old new]", got)
	}
}

func TestBotIDFromToken(t *testing.T) {
	id := "1530520100409573447"
	token := base64.RawStdEncoding.EncodeToString([]byte(id)) + ".Gabcde.xyz"
	if got := botIDFromToken(token); got.String() != id {
		t.Errorf("got %s, want %s", got, id)
	}
	if got := botIDFromToken("not a token"); got != 0 {
		t.Errorf("garbage token gave %s", got)
	}
}

func TestDiscordLinkIndex(t *testing.T) {
	f := newIngestFixture(t)
	for i := 0; i < 2; i++ { // idempotent
		if err := ensureDiscordLinkIndex(f.app); err != nil {
			t.Fatalf("ensure (run %d): %v", i+1, err)
		}
	}
	col, _ := f.app.FindCollectionByNameOrId("contents")
	n := 0
	for _, idx := range col.Indexes {
		if strings.Contains(idx, discordLinkIndexName) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d copies of %s, want 1", n, discordLinkIndexName)
	}
	var plan []struct {
		Detail string `db:"detail"`
	}
	if err := f.app.DB().NewQuery("EXPLAIN QUERY PLAN SELECT id FROM contents WHERE discord = 'x'").All(&plan); err != nil {
		t.Fatal(err)
	}
	if len(plan) == 0 || !strings.Contains(plan[0].Detail, discordLinkIndexName) {
		t.Errorf("the lookup doesn't use the index: %+v", plan)
	}
}

// ─── Manual requests ("Ingest this message" from the other bot) ─────────────

const (
	testModID   = "1000000000000000111"
	testModRole = "1000000000000000222"
)

var modRoles = []string{testModRole}

// linkedUploader makes an uploader whose account may upload, as the manual
// paths require of the person asking.
func (f *ingestFixture) linkedUploader(t *testing.T, name string, canUpload bool) {
	t.Helper()
	users, _ := f.app.FindCollectionByNameOrId("users")
	u := core.NewRecord(users)
	u.Set("email", name+"@example.com")
	u.SetPassword("password123456")
	u.Set("canUpload", canUpload)
	mustSaveRecord(t, f.app, u)
	uploaders, _ := f.app.FindCollectionByNameOrId("uploaders")
	up := core.NewRecord(uploaders)
	up.Set("name", name)
	up.Set("user", u.Id)
	mustSaveRecord(t, f.app, up)
}

func manualBody(m message, requester string, roles []string, idols []string, extra map[string]any) string {
	var msg map[string]any
	_ = json.Unmarshal([]byte(body(m, request{})), &msg)
	req := map[string]any{
		"message":      msg["message"],
		"bot_user_id":  testBotUser,
		"requested_by": map[string]any{"id": testModID, "username": requester, "roles": roles},
		"idols":        idols,
	}
	for k, v := range extra {
		req[k] = v
	}
	out, _ := json.Marshal(req)
	return string(out)
}

func (f *ingestFixture) manual(t *testing.T, b string) (int, ingestResponse, string) {
	t.Helper()
	return f.do(t, http.MethodPost, "/api/ingest/discord/manual", b, "Bearer "+testToken)
}

// withIngestRoles sets DISCORD_INGEST_ROLE_IDS for one test.
func withIngestRoles(t *testing.T, ids ...string) {
	t.Helper()
	real := manualIngestRoleIDs
	manualIngestRoleIDs = map[snowflake.ID]bool{}
	for _, id := range ids {
		manualIngestRoleIDs[snowflake.MustParse(id)] = true
	}
	t.Cleanup(func() { manualIngestRoleIDs = real })
}

func TestManualIngest(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)
	withIngestRoles(t, testModRole)

	// Any channel, allowlisted or not: somebody named this message and asked.
	const id = "2000000000000000300"
	m := message{id: id, channel: "1000000000000000777", content: "no ping at all", files: []string{"a.png", "b.png"}}
	code, resp, raw := f.manual(t, manualBody(m, "mod", []string{testModRole}, []string{"Yujin"}, map[string]any{"tags": []string{"fancam"}}))
	expect(t, "manual", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if resp.Created != 2 || resp.Reply != "✅ Ingested 2 item(s)." || !strings.Contains(resp.Set, "/set/") {
		t.Errorf("answer %+v", resp)
	}
	link := fmt.Sprintf("https://discord.com/channels/%s/%s/%s", testGuild, "1000000000000000777", id)
	post, err := f.app.FindFirstRecordByFilter("contents", "discord = {:link}", dbx.Params{"link": link})
	if err != nil {
		t.Fatalf("no post: %v", err)
	}
	// The person who asked is credited, not the author.
	if got := f.uploaderOf(t, post); got != "mod" {
		t.Errorf("credited %q, want the requester (mod)", got)
	}

	// Asking again within the hour gets the same answer.
	if code, again, raw := f.manual(t, manualBody(m, "mod", []string{testModRole}, []string{"Yujin"}, nil)); code != http.StatusOK || again.Status != string(statusIngested) {
		t.Errorf("repeat: %d %+v — %s", code, again, raw)
	}
	// The GET has it too.
	if code, got, raw := f.do(t, http.MethodGet, "/api/ingest/discord/manual/"+id, "", "Bearer "+testToken); code != http.StatusOK || got.Status != string(statusIngested) {
		t.Errorf("status lookup: %d %+v — %s", code, got, raw)
	}
}

func TestManualIngestGates(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)
	f.linkedUploader(t, "nopermission", false)
	withIngestRoles(t, testModRole)
	m := func(id string) message { return message{id: id, files: []string{"a.png"}} }

	code, resp, raw := f.manual(t, manualBody(m("2000000000000000310"), "mod", []string{}, []string{"Yujin"}, nil))
	expect(t, "without the role", code, resp, raw, http.StatusOK, string(statusDenied), reasonNoPermission, "")
	code, resp, raw = f.manual(t, manualBody(m("2000000000000000311"), "stranger", []string{testModRole}, []string{"Yujin"}, nil))
	expect(t, "not an uploader", code, resp, raw, http.StatusOK, string(statusDenied), reasonCannotUpload, "")
	code, resp, raw = f.manual(t, manualBody(m("2000000000000000312"), "nopermission", []string{testModRole}, []string{"Yujin"}, nil))
	expect(t, "account without upload permission", code, resp, raw, http.StatusOK, string(statusDenied), reasonCannotUpload, "")
	code, resp, raw = f.manual(t, manualBody(m("2000000000000000313"), "mod", []string{testModRole}, []string{"Nobody"}, nil))
	expect(t, "unknown idol", code, resp, raw, http.StatusOK, string(statusFailed), reasonUnknownIdols, "")
	if resp.Reply != "❌ I don't know: Nobody." {
		t.Errorf("reply %q", resp.Reply)
	}
	code, resp, raw = f.manual(t, manualBody(m("2000000000000000314"), "mod", []string{testModRole}, []string{"Yujin"},
		map[string]any{"set": "https://goyangi.pics/set/does-not-exist"}))
	expect(t, "unknown set", code, resp, raw, http.StatusOK, string(statusFailed), reasonBadSet, "")
	if n := f.count(t, "contents"); n != 0 {
		t.Errorf("%d posts created by refused requests", n)
	}

	// Request problems are 400s, not answers for the person.
	without := func(key string) string {
		var req map[string]any
		_ = json.Unmarshal([]byte(manualBody(m("2000000000000000315"), "mod", modRoles, []string{"Yujin"}, nil)), &req)
		delete(req, key)
		b, _ := json.Marshal(req)
		return string(b)
	}
	for key, want := range map[string]string{"requested_by": "requested_by", "bot_user_id": "bot_user_id", "message": "message"} {
		code, _, raw := f.manual(t, without(key))
		if code != http.StatusBadRequest || !strings.Contains(raw, want) {
			t.Errorf("without %s: %d, want a 400 about it — %s", key, code, raw)
		}
	}
}

func TestManualIngestJoinsASet(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)

	// A set made by a first request, then a second request adds to it.
	code, first, raw := f.manual(t, manualBody(message{id: "2000000000000000320", files: []string{"a.png"}}, "mod", modRoles, []string{"Yujin"}, nil))
	expect(t, "first", code, first, raw, http.StatusOK, string(statusIngested), "", "success")
	code, second, raw := f.manual(t, manualBody(message{id: "2000000000000000321", files: []string{"b.png"}}, "mod", modRoles, []string{"Yujin"},
		map[string]any{"set": first.Set}))
	expect(t, "second, into the first's set", code, second, raw, http.StatusOK, string(statusIngested), "", "success")
	if second.Set != first.Set || f.count(t, "contents_sets") != 1 {
		t.Errorf("second went to %q, want %q (and one set in all)", second.Set, first.Set)
	}
}

func TestManualIngestDryRun(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)
	code, resp, raw := f.manual(t, manualBody(message{id: "2000000000000000330", files: []string{"a.png"}}, "mod", modRoles, []string{"Yujin"},
		map[string]any{"dry_run": true}))
	if code != http.StatusOK || !resp.DryRun || resp.Status != string(statusIngested) || resp.Preview == nil ||
		resp.Preview.Trigger != "manual" || resp.Preview.Uploader != "mod" || len(resp.Preview.Items) != 1 {
		t.Fatalf("dry run: %d %+v — %s", code, resp, raw)
	}
	if n := f.count(t, "contents") + f.count(t, "contents_sets"); n != 0 || f.downloads.Load() != 0 {
		t.Errorf("a dry run left %d records and made %d downloads", n, f.downloads.Load())
	}
}

// An anonymous-post feature usually posts through a webhook; the forwarding
// bot can still say whose post it is.
func TestIngestAPIRelayThroughAWebhook(t *testing.T) {
	f := newIngestFixture(t)
	var req map[string]any
	_ = json.Unmarshal([]byte(body(message{id: "2000000000000000340", author: "1000000000000000444", username: "Anonymous",
		mentionRoles: []string{testRole}, files: []string{"a.png"}},
		request{roles: yujin, relay: map[string]any{"user": map[string]any{"id": "1000000000000000055", "username": "hyein"}}})), &req)
	req["message"].(map[string]any)["webhook_id"] = "1000000000000000444"
	b, _ := json.Marshal(req)

	code, resp, raw := f.post(t, string(b))
	expect(t, "webhook relay", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if got := f.uploaderOf(t, f.postFor(t, "2000000000000000340")); got != "hyein" {
		t.Errorf("credited %q, want hyein", got)
	}
}

// A message already on the site isn't archived again on request.
func TestManualIngestRefusesWhatIsAlreadyArchived(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)
	const id = "2000000000000000350"
	code, resp, raw := f.post(t, body(message{id: id, mentionRoles: []string{testRole}, files: []string{"a.png"}}, request{roles: yujin}))
	expect(t, "archived automatically", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")

	code, resp, raw = f.manual(t, manualBody(message{id: id, files: []string{"a.png"}}, "mod", modRoles, []string{"Yujin"}, nil))
	expect(t, "asked for afterwards", code, resp, raw, http.StatusOK, string(statusDuplicate), reasonAlreadyArchived, "")
	if n := f.count(t, "contents"); n != 1 {
		t.Errorf("%d posts, want 1", n)
	}
}

// A message that mentions the bot without idol/group lines is still a
// follow-up: a bot that does other things gets mentioned for those too.
func TestMentionWithoutLinesCanBeAFollowUp(t *testing.T) {
	f := newIngestFixture(t)
	code, resp, raw := f.post(t, body(message{id: "2000000000000000400", mentionRoles: []string{testRole}, files: []string{"a.png"}}, request{roles: yujin}))
	expect(t, "opener", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	code, resp, raw = f.post(t, body(message{id: "2000000000000000401", content: "<@" + testBotUser + "> /something else", files: []string{"b.png"}}, request{}))
	expect(t, "follow-up mentioning the bot", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if f.postFor(t, "2000000000000000401").GetString("set") != f.postFor(t, "2000000000000000400").GetString("set") {
		t.Error("the follow-up didn't join the opener's set")
	}
}

// A follow-up whose items all fail hasn't joined anything, so it doesn't use
// one of the three slots.
func TestFailedFollowUpKeepsItsSlot(t *testing.T) {
	f := newIngestFixture(t)
	code, resp, raw := f.post(t, body(message{id: "2000000000000000410", mentionRoles: []string{testRole}, files: []string{"a.png"}}, request{roles: yujin}))
	expect(t, "opener", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	code, resp, raw = f.post(t, body(message{id: "2000000000000000411", files: []string{"missing.png"}}, request{}))
	expect(t, "failed follow-up", code, resp, raw, http.StatusOK, string(statusFailed), reasonNothingSaved, "")
	for i, id := range []string{"2000000000000000412", "2000000000000000413", "2000000000000000414"} {
		code, resp, raw = f.post(t, body(message{id: id, files: []string{fmt.Sprintf("f%d.png", i)}}, request{}))
		expect(t, "follow-up "+id, code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	}
}

// During a shadow run goyangi's own bot usually got there first; the dry run
// still shows what it would have done, so the two can be compared.
func TestDryRunOfATakenMessageKeepsThePreview(t *testing.T) {
	f := newIngestFixture(t)
	m := message{id: "2000000000000000420", content: "filetype: video", mentionRoles: []string{testRole}, files: []string{"a.png", "b.png"}}
	code, resp, raw := f.post(t, body(m, request{roles: yujin}))
	expect(t, "taken for real", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")

	code, resp, raw = f.post(t, body(m, request{roles: yujin, dryRun: true}))
	if code != http.StatusOK || resp.Status != string(statusDuplicate) || resp.Preview == nil || resp.Total != 2 {
		t.Fatalf("dry run of a taken message: %d %+v — %s", code, resp, raw)
	}
	if resp.Preview.Title != "Yujin - IVE" || resp.Preview.Items[0].Filetype != "video" {
		t.Errorf("preview %+v, want the real title and the filetype line applied", resp.Preview)
	}
}

func TestDryRunDoesntRegisterTheBot(t *testing.T) {
	f := newIngestFixture(t)
	f.post(t, body(message{id: "2000000000000000430", files: []string{"a.png"}}, request{dryRun: true}))
	forwardersMu.Lock()
	known := forwarders[snowflake.MustParse(testBotUser)]
	forwardersMu.Unlock()
	if known {
		t.Error("a dry run made goyangi treat the bot as one of its own")
	}
}

func TestPinnedForwardingBots(t *testing.T) {
	f := newIngestFixture(t)
	ingestBotIDs = map[snowflake.ID]bool{snowflake.MustParse(testBotUser): true}
	code, _, raw := f.post(t, body(message{id: "2000000000000000440", files: []string{"a.png"}}, request{botID: testAuthor}))
	if code != http.StatusBadRequest || !strings.Contains(raw, "bot_user_id") {
		t.Errorf("an unlisted bot_user_id: %d, want a 400 — %s", code, raw)
	}
	if code, _, raw := f.post(t, body(message{id: "2000000000000000441", files: []string{"a.png"}}, request{})); code != http.StatusOK {
		t.Errorf("the listed bot: %d — %s", code, raw)
	}
}

// A message queued behind a slow one in its channel answers 202 quickly, so a
// client sending a channel's messages one at a time isn't held 25 s for each.
func TestQueuedMessageAnswersQuickly(t *testing.T) {
	f := newIngestFixture(t)
	ingestWait, queuedWait = 5*time.Second, 50*time.Millisecond

	go f.post(t, body(message{id: "2000000000000000450", mentionRoles: []string{testRole}, files: []string{"slow.png"}}, request{roles: yujin}))
	for deadline := time.Now().Add(2 * time.Second); ; {
		ingestJobsMu.Lock()
		n := ingestPending
		ingestJobsMu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	start := time.Now()
	code, resp, raw := f.post(t, body(message{id: "2000000000000000451", files: []string{"b.png"}}, request{}))
	if code != http.StatusAccepted || resp.Status != "processing" || time.Since(start) > time.Second {
		t.Errorf("queued message: %d %q after %v, want a quick 202 — %s", code, resp.Status, time.Since(start), raw)
	}
	f.unblock()
	if v := f.await(t, "2000000000000000451"); v.Status != string(statusIngested) {
		t.Errorf("queued message's verdict: %+v", v)
	}
}

// Dry runs make no network calls: imgur albums are listed, not looked up.
func TestDryRunsDontLookUpAlbums(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)
	var lookups atomic.Int64
	real := resolveImgurAlbum
	resolveImgurAlbum = func(string) []MediaItem { lookups.Add(1); return nil }
	t.Cleanup(func() { resolveImgurAlbum = real })

	album := "https://imgur.com/a/AbCdE12"
	code, resp, raw := f.post(t, body(message{id: "2000000000000000460", content: album, mentionRoles: []string{testRole}}, request{roles: yujin, dryRun: true}))
	if code != http.StatusOK || resp.Preview == nil || len(resp.Preview.Items) != 1 || resp.Preview.Items[0].Filetype != "album" {
		t.Errorf("forwarded dry run: %d %+v — %s", code, resp, raw)
	}
	code, resp, raw = f.manual(t, manualBody(message{id: "2000000000000000461", content: album}, "mod", modRoles, []string{"Yujin"}, map[string]any{"dry_run": true}))
	if code != http.StatusOK || resp.Preview == nil || len(resp.Preview.Items) != 1 || resp.Preview.Items[0].Filetype != "album" {
		t.Errorf("manual dry run: %d %+v — %s", code, resp, raw)
	}
	if n := lookups.Load(); n != 0 {
		t.Errorf("%d album lookups in dry runs, want none", n)
	}
}

// The person who asked is credited, whatever an `uploader:` line in the
// message says: that line would name someone nobody checked.
func TestManualIngestCreditsTheRequesterOnly(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)
	const id = "2000000000000000500"
	code, resp, raw := f.manual(t, manualBody(message{id: id, content: "uploader: somebodyelse", files: []string{"a.png"}}, "mod", modRoles, []string{"Yujin"}, nil))
	expect(t, "with an uploader line", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
	if got := f.uploaderOf(t, f.postFor(t, id)); got != "mod" {
		t.Errorf("credited %q, want the requester (mod)", got)
	}
	if rec, _ := findUploaderByNames("somebodyelse"); rec != nil {
		t.Error("an uploader was created for the name in the message")
	}
}

// Matched on the unique username, never on a display name anyone can choose.
func TestManualIngestIgnoresDisplayNames(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)
	var req map[string]any
	_ = json.Unmarshal([]byte(manualBody(message{id: "2000000000000000510", files: []string{"a.png"}}, "evil", modRoles, []string{"Yujin"}, nil)), &req)
	req["requested_by"].(map[string]any)["global_name"] = "mod"
	b, _ := json.Marshal(req)
	code, resp, raw := f.manual(t, string(b))
	expect(t, "display name set to an uploader's", code, resp, raw, http.StatusOK, string(statusDenied), reasonCannotUpload, "")
}

func TestManualIngestFailsClosed(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)

	// A server not on the list: a role there proves nothing about this one.
	manualGuildIDs = map[snowflake.ID]bool{snowflake.MustParse("1000000000000000999"): true}
	code, resp, raw := f.manual(t, manualBody(message{id: "2000000000000000520", files: []string{"a.png"}}, "mod", modRoles, []string{"Yujin"}, nil))
	expect(t, "server not listed", code, resp, raw, http.StatusOK, string(statusDenied), reasonServerNotListed, "")
	manualGuildIDs = map[snowflake.ID]bool{snowflake.MustParse(testGuild): true}

	// No roles configured: nobody may ask.
	manualIngestRoleIDs = map[snowflake.ID]bool{}
	code, resp, raw = f.manual(t, manualBody(message{id: "2000000000000000521", files: []string{"a.png"}}, "mod", modRoles, []string{"Yujin"}, nil))
	expect(t, "no roles configured", code, resp, raw, http.StatusOK, string(statusDenied), reasonNoPermission, "")
	if n := f.count(t, "contents"); n != 0 {
		t.Errorf("%d posts created", n)
	}
}

// An answer about one person isn't served to the next.
func TestManualAnswersArePerRequest(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "blockedmod", true)
	f.linkedUploader(t, "mod", true)
	rec, _ := findUploaderByNames("blockedmod")
	rec.Set("blockIngest", true)
	mustSaveRecord(t, f.app, rec)

	m := message{id: "2000000000000000530", files: []string{"a.png"}}
	code, resp, raw := f.manual(t, manualBody(m, "blockedmod", modRoles, []string{"Yujin"}, nil))
	expect(t, "blocked requester", code, resp, raw, http.StatusOK, string(statusSkipped), reasonBlocked, "skipped")
	code, resp, raw = f.manual(t, manualBody(m, "mod", modRoles, []string{"Yujin"}, nil))
	expect(t, "next requester", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")
}

// A follow-up resent after a restart has lost what tied it to its set, but
// its posts are on the site: that's a duplicate, not an unarchivable message.
func TestResentFollowUpIsADuplicate(t *testing.T) {
	f := newIngestFixture(t)
	f.post(t, body(message{id: "2000000000000000540", mentionRoles: []string{testRole}, files: []string{"a.png"}}, request{roles: yujin}))
	followUp := body(message{id: "2000000000000000541", files: []string{"b.png"}}, request{})
	code, resp, raw := f.post(t, followUp)
	expect(t, "follow-up", code, resp, raw, http.StatusOK, string(statusIngested), "", "success")

	// A restart: no claims, no jobs, no chains.
	processedMu.Lock()
	processedMsgs = map[string]time.Time{}
	processedMu.Unlock()
	ingestJobsMu.Lock()
	ingestJobs = map[string]*ingestJob{}
	ingestJobsMu.Unlock()
	chainMu.Lock()
	chains = map[string]chainEntry{}
	chainMu.Unlock()

	code, resp, raw = f.post(t, followUp)
	expect(t, "resent follow-up", code, resp, raw, http.StatusOK, string(statusDuplicate), reasonAlreadyArchived, "")
}

func TestManualDryRunCountsAndKeepsPreviewWhenTaken(t *testing.T) {
	f := newIngestFixture(t)
	f.linkedUploader(t, "mod", true)
	m := message{id: "2000000000000000550", files: []string{"a.png", "b.png"}}
	code, resp, raw := f.manual(t, manualBody(m, "mod", modRoles, []string{"Yujin"}, map[string]any{"dry_run": true}))
	if code != http.StatusOK || resp.Total != 2 || resp.Preview == nil {
		t.Fatalf("dry run: %d %+v — %s", code, resp, raw)
	}
	claimMessage("2000000000000000550") // in flight elsewhere
	code, resp, raw = f.manual(t, manualBody(m, "mod", modRoles, []string{"Yujin"}, map[string]any{"dry_run": true}))
	if code != http.StatusOK || resp.Status != string(statusDuplicate) || resp.Preview == nil {
		t.Errorf("dry run of a claimed message: %d %+v — %s", code, resp, raw)
	}
}
