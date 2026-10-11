package bot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/disgoorg/snowflake/v2"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

const (
	testToken   = "test-ingest-token"
	testGuild   = "1000000000000000001"
	testChannel = "1000000000000000002"
	testBotUser = "1000000000000000003"
	testRole    = "1000000000000000004"
)

// A 1×1 PNG, so the download yields a real image.
var testPNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0xf8, 0xcf, 0xc0, 0xf0,
	0x1f, 0x00, 0x05, 0x00, 0x01, 0xff, 0x89, 0x99, 0x3d, 0x1d, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45,
	0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

type ingestFixture struct {
	app   *tests.TestApp
	mux   http.Handler
	media string // base URL serving test media
}

// newIngestFixture is a test app with the real schema, IVE and Yujin in the
// directory, a media server, the test channel allowlisted, and the ingest
// routes mounted with testToken.
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

	// Package state from other tests or runs: the app, the directory cache,
	// the claims, open sets and chains, and the job registry.
	Init(app)
	directoryCache = nil
	setRegistryMu.Lock()
	setRegistry = map[string]setEntry{}
	setRegistryMu.Unlock()
	chainMu.Lock()
	chains = map[string]chainEntry{}
	chainMu.Unlock()
	processedMu.Lock()
	processedMsgs = map[string]time.Time{}
	processedMu.Unlock()
	ingestJobsMu.Lock()
	ingestJobs = map[string]*ingestJob{}
	ingestJobsMu.Unlock()

	channel, _ := snowflake.Parse(testChannel)
	allowedChannelIDs[channel] = true
	t.Cleanup(func() { delete(allowedChannelIDs, channel) })

	// The real client refuses loopback (safehttp.go); the test server is loopback.
	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(testPNG)
	}))
	t.Cleanup(media.Close)
	realClient := mediaDownloadClient
	mediaDownloadClient = media.Client()
	t.Cleanup(func() { mediaDownloadClient = realClient })

	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	f := &ingestFixture{app: app, media: media.URL}
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

// message builds a Discord message object the way the gateway delivers it.
type message struct {
	id, channel, author, username, content string
	mentionRoles                           []string
	attachment                             bool
}

func (f *ingestFixture) body(m message, roles map[string]string, botUserID string) string {
	if m.channel == "" {
		m.channel = testChannel
	}
	if m.author == "" {
		m.author = "1000000000000000099"
	}
	if m.username == "" {
		m.username = "nabi"
	}
	msg := map[string]any{
		"id":            m.id,
		"channel_id":    m.channel,
		"guild_id":      testGuild,
		"author":        map[string]any{"id": m.author, "username": m.username},
		"content":       m.content,
		"mention_roles": append([]string{}, m.mentionRoles...),
		"attachments":   []any{},
		"embeds":        []any{},
		"timestamp":     "2026-10-11T12:00:00.000000+00:00",
		"type":          0,
	}
	if m.attachment {
		msg["attachments"] = []any{map[string]any{
			"id":           "1000000000000000500",
			"filename":     "yujin.png",
			"content_type": "image/png",
			"size":         len(testPNG),
			"url":          f.media + "/yujin.png",
			"proxy_url":    f.media + "/yujin.png",
		}}
	}
	out, _ := json.Marshal(map[string]any{"message": msg, "roles": roles, "bot_user_id": botUserID})
	return string(out)
}

func (f *ingestFixture) do(t *testing.T, method, path, body, token string) (int, ingestResponse, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	var resp ingestResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp, rec.Body.String()
}

func (f *ingestFixture) post(t *testing.T, body string) (int, ingestResponse, string) {
	t.Helper()
	return f.do(t, http.MethodPost, "/api/ingest/discord/messages", body, testToken)
}

func jumpLink(messageID string) string {
	return fmt.Sprintf("https://discord.com/channels/%s/%s/%s", testGuild, testChannel, messageID)
}

func TestIngestAPIRequiresToken(t *testing.T) {
	f := newIngestFixture(t)
	body := f.body(message{id: "2000000000000000001"}, nil, "")
	for name, token := range map[string]string{"no token": "", "wrong token": "nope"} {
		if code, _, raw := f.do(t, http.MethodPost, "/api/ingest/discord/messages", body, token); code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401 — %s", name, code, raw)
		}
	}
	if code, _, raw := f.do(t, http.MethodGet, "/api/ingest/discord/messages/1", "", ""); code != http.StatusUnauthorized {
		t.Errorf("status lookup without token: %d, want 401 — %s", code, raw)
	}
}

func TestIngestAPIRejectsMalformedRequests(t *testing.T) {
	f := newIngestFixture(t)
	cases := map[string]string{
		"not json":   "{",
		"no message": `{"roles":{}}`,
		"no guild":   `{"message":{"id":"2000000000000000002","channel_id":"` + testChannel + `","author":{"id":"5","username":"x"}}}`,
		// A role ping whose name wasn't sent can't be attributed.
		"unnamed pinged role": f.body(message{id: "2000000000000000003", mentionRoles: []string{testRole}, attachment: true}, nil, ""),
	}
	for name, body := range cases {
		if code, _, raw := f.post(t, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400 — %s", name, code, raw)
		}
	}
}

func TestIngestAPIIgnoresWhatTheBotWouldIgnore(t *testing.T) {
	f := newIngestFixture(t)
	roles := map[string]string{testRole: "Yujin [IVE]"}
	cases := map[string]string{
		"channel outside the allowlist": f.body(message{id: "2000000000000000010", channel: "1000000000000000777",
			mentionRoles: []string{testRole}, attachment: true}, roles, ""),
		"no media": f.body(message{id: "2000000000000000011", mentionRoles: []string{testRole}, content: "hi"}, roles, ""),
		"the forwarding bot's own message": f.body(message{id: "2000000000000000012", author: testBotUser,
			mentionRoles: []string{testRole}, attachment: true}, roles, testBotUser),
	}
	for name, body := range cases {
		code, resp, raw := f.post(t, body)
		if code != http.StatusOK || resp.Status != string(statusIgnored) || resp.Reaction != "" {
			t.Errorf("%s: %d %s, want 200 ignored with no reaction — %s", name, code, resp.Status, raw)
		}
	}
	if n, _ := f.app.CountRecords("contents"); n != 0 {
		t.Errorf("%d posts created, want none", n)
	}
}

func TestIngestAPIRolePingIngests(t *testing.T) {
	f := newIngestFixture(t)
	const id = "2000000000000000020"
	body := f.body(message{id: id, mentionRoles: []string{testRole}, attachment: true},
		map[string]string{testRole: "Yujin [IVE]"}, testBotUser)

	code, resp, raw := f.post(t, body)
	if code != http.StatusOK || resp.Status != string(statusIngested) {
		t.Fatalf("status %d %q, want 200 ingested — %s", code, resp.Status, raw)
	}
	if resp.Reaction != "success" || resp.Created != 1 || resp.Total != 1 || !strings.Contains(resp.Set, "/set/") {
		t.Errorf("verdict %+v, want success, 1/1 and a set link", resp)
	}

	post, err := f.app.FindFirstRecordByFilter("contents", "discord = {:link}", dbx.Params{"link": jumpLink(id)})
	if err != nil {
		t.Fatalf("no post carries the message link: %v", err)
	}
	if post.GetString("set") == "" || !strings.HasSuffix(resp.Set, post.GetString("set")) {
		t.Errorf("post set %q doesn't match the reported %q", post.GetString("set"), resp.Set)
	}
	// Credited the way our own bot credits: the author's username, minted as an
	// uploader on first sight.
	up, err := f.app.FindRecordById("uploaders", post.GetString("uploader"))
	if err != nil || up.GetString("name") != "nabi" {
		t.Errorf("uploader %v (%v), want nabi", up, err)
	}

	// A retry gets the original verdict back, not a second ingestion.
	if code, again, raw := f.post(t, body); code != http.StatusOK || again.Status != string(statusIngested) {
		t.Errorf("retry: %d %q, want the original verdict — %s", code, again.Status, raw)
	}
	// The verdict can also be collected.
	if code, got, raw := f.do(t, http.MethodGet, "/api/ingest/discord/messages/"+id, "", testToken); code != http.StatusOK || got.Status != string(statusIngested) {
		t.Errorf("status lookup: %d %q — %s", code, got.Status, raw)
	}

	// After a restart (no claims, no jobs) the archive itself says no.
	processedMu.Lock()
	processedMsgs = map[string]time.Time{}
	processedMu.Unlock()
	ingestJobsMu.Lock()
	ingestJobs = map[string]*ingestJob{}
	ingestJobsMu.Unlock()
	if code, dup, raw := f.post(t, body); code != http.StatusOK || dup.Status != string(statusDuplicate) || dup.Reaction != "" {
		t.Errorf("after restart: %d %q, want duplicate — %s", code, dup.Status, raw)
	}
	if n, _ := f.app.CountRecords("contents"); n != 1 {
		t.Errorf("%d posts, want exactly 1", n)
	}
}

func TestIngestAPISkipsLikeTheBot(t *testing.T) {
	f := newIngestFixture(t)
	roles := map[string]string{testRole: "Yujin [IVE]"}

	// A role ping plus an @-mention of the bot is a re-announcement.
	code, resp, raw := f.post(t, f.body(message{id: "2000000000000000030", content: "<@" + testBotUser + "> look",
		mentionRoles: []string{testRole}, attachment: true}, roles, testBotUser))
	if code != http.StatusOK || resp.Status != string(statusSkipped) || resp.Reaction != "skipped" {
		t.Errorf("ping + mention: %d %+v — %s", code, resp, raw)
	}

	// An uploader who turned off automatic import.
	uploaders, _ := f.app.FindCollectionByNameOrId("uploaders")
	u := core.NewRecord(uploaders)
	u.Set("name", "optedout")
	u.Set("skipDiscordImport", true)
	mustSaveRecord(t, f.app, u)
	code, resp, raw = f.post(t, f.body(message{id: "2000000000000000031", author: "1000000000000000098", username: "optedout",
		mentionRoles: []string{testRole}, attachment: true}, roles, testBotUser))
	if code != http.StatusOK || resp.Status != string(statusSkipped) || resp.Reaction != "skipped" {
		t.Errorf("opted out: %d %+v — %s", code, resp, raw)
	}

	if n, _ := f.app.CountRecords("contents"); n != 0 {
		t.Errorf("%d posts created, want none", n)
	}
}

func TestIngestAPISlowMessageAnswers202ThenVerdict(t *testing.T) {
	f := newIngestFixture(t)
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(testPNG)
	}))
	defer slow.Close()
	f.media = slow.URL
	mediaDownloadClient = slow.Client()

	realWait := ingestWait
	ingestWait = 50 * time.Millisecond
	t.Cleanup(func() { ingestWait = realWait })

	const id = "2000000000000000040"
	code, resp, raw := f.post(t, f.body(message{id: id, mentionRoles: []string{testRole}, attachment: true},
		map[string]string{testRole: "Yujin [IVE]"}, ""))
	if code != http.StatusAccepted || resp.Status != "processing" {
		t.Fatalf("slow message: %d %q, want 202 processing — %s", code, resp.Status, raw)
	}
	if code, got, _ := f.do(t, http.MethodGet, "/api/ingest/discord/messages/"+id, "", testToken); code != http.StatusAccepted || got.Status != "processing" {
		t.Errorf("status while running: %d %q, want 202 processing", code, got.Status)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, got, raw := f.do(t, http.MethodGet, "/api/ingest/discord/messages/"+id, "", testToken)
		if code == http.StatusOK {
			if got.Status != string(statusIngested) || got.Created != 1 {
				t.Errorf("verdict %+v, want ingested 1 — %s", got, raw)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no verdict after release: %d %s", code, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestIngestAPIUnknownStatus(t *testing.T) {
	f := newIngestFixture(t)
	if code, _, raw := f.do(t, http.MethodGet, "/api/ingest/discord/messages/123", "", testToken); code != http.StatusNotFound {
		t.Errorf("status %d, want 404 — %s", code, raw)
	}
}

func TestRegisterIngestAPIOffWithoutToken(t *testing.T) {
	t.Setenv("INGEST_API_TOKEN", " , ")
	if got := ingestTokens(); len(got) != 0 {
		t.Errorf("tokens %q, want none", got)
	}
	t.Setenv("INGEST_API_TOKEN", "old, new")
	if got := ingestTokens(); len(got) != 2 || got[0] != "old" || got[1] != "new" {
		t.Errorf("tokens %q, want [old new]", got)
	}
}
