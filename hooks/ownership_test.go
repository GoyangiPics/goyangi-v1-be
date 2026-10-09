package hooks

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// Ownership tests drive real HTTP requests through PocketBase's router against
// the real schema, so the access rules, the request hooks and the model hooks
// all run exactly as they do in production.

type fixture struct {
	app            *tests.TestApp
	mux            http.Handler
	tok            map[string]string // "u1", "u2", "admin" → auth token
	up             map[string]string // same keys → uploader id
	idol, groupID  string
	users          map[string]*core.Record
	contentsByName map[string]string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	app := newSchemaApp(t)
	if err := ensureAccessRules(app); err != nil {
		t.Fatalf("rules: %v", err)
	}
	bindOwnershipHooks(app)
	RegisterUploaderGuards(app)
	RegisterSetRoutes(app)

	f := &fixture{
		app:   app,
		tok:   map[string]string{},
		up:    map[string]string{},
		users: map[string]*core.Record{},
	}

	users, _ := app.FindCollectionByNameOrId("users")
	uploaders, _ := app.FindCollectionByNameOrId("uploaders")
	for _, who := range []string{"u1", "u2", "admin"} {
		u := core.NewRecord(users)
		u.SetEmail(who + "@example.test")
		u.SetPassword("password123")
		u.Set("canUpload", true)
		u.Set("isAdmin", who == "admin")
		mustSave(t, app, u)
		token, err := u.NewAuthToken()
		if err != nil {
			t.Fatalf("token: %v", err)
		}
		f.tok[who] = token
		f.users[who] = u

		up := core.NewRecord(uploaders)
		up.Set("name", who)
		up.Set("user", u.Id)
		mustSave(t, app, up)
		f.up[who] = up.Id
	}

	groups, _ := app.FindCollectionByNameOrId("groups")
	g := core.NewRecord(groups)
	g.Set("name", "IVE")
	g.Set("code", "ive")
	mustSave(t, app, g)
	f.groupID = g.Id

	idols, _ := app.FindCollectionByNameOrId("groups_idols")
	i := core.NewRecord(idols)
	i.Set("name", "Yujin")
	i.Set("code", "yujin")
	i.Set("group", g.Id)
	mustSave(t, app, i)
	f.idol = i.Id

	// One router for the whole fixture, built the way tests.ApiScenario builds
	// it — which can't be reused across requests on one app.
	router, err := apis.NewRouter(app)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	serve := &core.ServeEvent{App: app, Router: router}
	if err := app.OnServe().Trigger(serve, func(e *core.ServeEvent) error {
		mux, err := e.Router.BuildMux()
		f.mux = mux
		return err
	}); err != nil {
		t.Fatalf("serve: %v", err)
	}
	return f
}

func mustSave(t *testing.T, app core.App, r *core.Record) {
	t.Helper()
	if err := app.SaveNoValidate(r); err != nil {
		t.Fatalf("save %s: %v", r.Collection().Name, err)
	}
}

// set makes a set credited to the given uploaders (as the upload page would).
func (f *fixture) set(t *testing.T, owners ...string) string {
	t.Helper()
	col, _ := f.app.FindCollectionByNameOrId("contents_sets")
	s := core.NewRecord(col)
	s.Set("title", "260101 set")
	s.Set("idol", []string{f.idol})
	s.Set("group", []string{f.groupID})
	ids := make([]string, 0, len(owners))
	for _, o := range owners {
		ids = append(ids, f.up[o])
	}
	s.Set("uploader", ids)
	mustSave(t, f.app, s)
	return s.Id
}

// post makes a post by `owner` in `set` ("" for none).
func (f *fixture) post(t *testing.T, owner, set string) string {
	t.Helper()
	col, _ := f.app.FindCollectionByNameOrId("contents")
	p := core.NewRecord(col)
	p.Set("title", "post")
	p.Set("idol", []string{f.idol})
	p.Set("group", []string{f.groupID})
	p.Set("filetype", "image")
	if owner != "" {
		p.Set("uploader", f.up[owner])
	}
	p.Set("set", set)
	mustSave(t, f.app, p)
	return p.Id
}

func (f *fixture) do(t *testing.T, name, method, url, body, who string, status int, contains ...string) {
	t.Helper()
	req := httptest.NewRequest(method, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.Header.Set("Authorization", f.tok[who])
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Errorf("%s: status %d, want %d — %s", name, rec.Code, status, rec.Body.String())
		return
	}
	got := strings.ReplaceAll(rec.Body.String(), " ", "")
	for _, want := range contains {
		if !strings.Contains(got, want) {
			t.Errorf("%s: body lacks %q — %s", name, want, got)
		}
	}
}

func (f *fixture) record(t *testing.T, collection, id string) *core.Record {
	t.Helper()
	r, err := f.app.FindRecordById(collection, id)
	if err != nil {
		return nil
	}
	return r
}

func posts(id string) string { return "/api/collections/contents/records/" + id }
func sets(id string) string  { return "/api/collections/contents_sets/records/" + id }

func TestOwnerMovesPosts(t *testing.T) {
	f := newFixture(t)
	mine := f.set(t, "u1")
	other := f.set(t, "u1")
	theirs := f.set(t, "u2")
	f.post(t, "u2", theirs)
	p := f.post(t, "u1", mine)
	keep := f.post(t, "u1", other)
	_ = keep

	f.do(t, "into someone else's set", http.MethodPatch, posts(p), `{"set":"`+theirs+`"}`, "u1", 404)
	f.do(t, "into my other set", http.MethodPatch, posts(p), `{"set":"`+other+`"}`, "u1", 200)

	if f.record(t, "contents_sets", mine) != nil {
		t.Error("the set the last post left should have been deleted")
	}
	f.do(t, "out of any set", http.MethodPatch, posts(p), `{"set":""}`, "u1", 200)
	if f.record(t, "contents_sets", other) == nil {
		t.Error("a set that still has a post must stay")
	}
	f.do(t, "admin moves anyone's post anywhere", http.MethodPatch, posts(p), `{"set":"`+theirs+`"}`, "admin", 200)
}

func TestOwnerCannotRecreditOrRepointPosts(t *testing.T) {
	f := newFixture(t)
	p := f.post(t, "u1", "")

	f.do(t, "re-credit", http.MethodPatch, posts(p), `{"uploader":"`+f.up["u2"]+`"}`, "u1", 404)
	f.do(t, "pipeline field", http.MethodPatch, posts(p), `{"preview":"https://evil.example/x.avif"}`, "u1", 404)
	f.do(t, "views", http.MethodPatch, posts(p), `{"views":9999}`, "u1", 404)
	f.do(t, "same uploader resent is fine", http.MethodPatch, posts(p),
		`{"title":"renamed","uploader":"`+f.up["u1"]+`"}`, "u1", 200, `"renamed"`)
	f.do(t, "non-owner edit", http.MethodPatch, posts(p), `{"title":"mine now"}`, "u2", 404)
}

func TestCreateCreditsTheCaller(t *testing.T) {
	f := newFixture(t)
	body := func(uploader string) string {
		return `{"title":"t","filetype":"image","idol":["` + f.idol + `"],"group":["` + f.groupID +
			`"],"uploader":"` + uploader + `"}`
	}
	f.do(t, "credited to someone else", http.MethodPost, "/api/collections/contents/records", body(f.up["u2"]), "u1", 400)
	f.do(t, "credited to me", http.MethodPost, "/api/collections/contents/records", body(f.up["u1"]), "u1", 200)

	setBody := func(uploaders string) string {
		return `{"title":"s","idol":["` + f.idol + `"],"group":["` + f.groupID + `"],"uploader":[` + uploaders + `]}`
	}
	f.do(t, "set naming someone else", http.MethodPost, "/api/collections/contents_sets/records",
		setBody(`"`+f.up["u2"]+`"`), "u1", 400)
	f.do(t, "set naming me and someone else", http.MethodPost, "/api/collections/contents_sets/records",
		setBody(`"`+f.up["u1"]+`","`+f.up["u2"]+`"`), "u1", 400)
	f.do(t, "set naming me", http.MethodPost, "/api/collections/contents_sets/records",
		setBody(`"`+f.up["u1"]+`"`), "u1", 200)
}

func TestSetUploadersAreDerived(t *testing.T) {
	f := newFixture(t)
	s := f.set(t, "u1")
	f.post(t, "u1", s)

	f.do(t, "joining someone's set by editing it", http.MethodPatch, sets(s),
		`{"uploader+":"`+f.up["u2"]+`"}`, "u2", 404)
	f.do(t, "co-owner can't rewrite the list either", http.MethodPatch, sets(s),
		`{"uploader":["`+f.up["u2"]+`"]}`, "u1", 404)
	f.do(t, "co-owner edits the set", http.MethodPatch, sets(s), `{"title":"260101 renamed"}`, "u1", 200)

	// Co-uploading by adding a post is how a second uploader joins.
	f.post(t, "u2", s)
	got := f.record(t, "contents_sets", s).GetStringSlice("uploader")
	if !slices.Equal(got, []string{f.up["u1"], f.up["u2"]}) {
		t.Errorf("uploaders = %v, want u1 then u2", got)
	}
}

func TestDeletingSharedSets(t *testing.T) {
	f := newFixture(t)
	shared := f.set(t, "u1")
	f.post(t, "u1", shared)
	theirPost := f.post(t, "u2", shared)

	f.do(t, "co-owner can't delete a set with others' posts", http.MethodDelete, sets(shared), "", "u1", 404)
	if f.record(t, "contents", theirPost) == nil {
		t.Fatal("their post must survive")
	}

	solo := f.set(t, "u1")
	soloPost := f.post(t, "u1", solo)
	f.do(t, "owner of every post deletes the set", http.MethodDelete, sets(solo), "", "u1", 204)
	if f.record(t, "contents", soloPost) != nil {
		t.Error("the set's posts go with it")
	}

	f.do(t, "admin deletes any set", http.MethodDelete, sets(shared), "", "admin", 204)
}

func TestDeletingTheLastPostRemovesTheSet(t *testing.T) {
	f := newFixture(t)
	s := f.set(t, "u1")
	a := f.post(t, "u1", s)
	b := f.post(t, "u1", s)

	f.do(t, "first of two", http.MethodDelete, posts(a), "", "u1", 204)
	if f.record(t, "contents_sets", s) == nil {
		t.Fatal("set still has a post")
	}
	f.do(t, "last one", http.MethodDelete, posts(b), "", "u1", 204)
	if f.record(t, "contents_sets", s) != nil {
		t.Error("an emptied set should be deleted")
	}
}

func TestLikesGuard(t *testing.T) {
	f := newFixture(t)
	p := f.post(t, "u1", "")
	likes, _ := f.app.FindCollectionByNameOrId("users_likes")
	like := func(who string) string {
		l := core.NewRecord(likes)
		l.Set("user", f.users[who].Id)
		l.Set("content", p)
		mustSave(t, f.app, l)
		return l.Id
	}
	mine, theirs := like("u2"), like("u1")

	f.do(t, "add someone else's like", http.MethodPatch, posts(p), `{"likes+":"`+theirs+`"}`, "u2", 403)
	f.do(t, "add my like", http.MethodPatch, posts(p), `{"likes+":"`+mine+`"}`, "u2", 200)

	f.do(t, "owner adds their like", http.MethodPatch, posts(p), `{"likes+":"`+theirs+`"}`, "u1", 200)
	f.do(t, "strip someone else's like", http.MethodPatch, posts(p), `{"likes-":"`+theirs+`"}`, "u2", 403)
	f.do(t, "remove my like", http.MethodPatch, posts(p), `{"likes-":"`+mine+`"}`, "u2", 200)
}

func TestAdminsReadReports(t *testing.T) {
	f := newFixture(t)
	p := f.post(t, "u1", "")
	reports, _ := f.app.FindCollectionByNameOrId("contents_reports")
	r := core.NewRecord(reports)
	r.Set("user", f.users["u2"].Id)
	r.Set("content", p)
	r.Set("type", "quality")
	mustSave(t, f.app, r)

	f.do(t, "admin lists", http.MethodGet, "/api/collections/contents_reports/records", "", "admin", 200, `"totalItems":1`)
	f.do(t, "reporter lists own", http.MethodGet, "/api/collections/contents_reports/records", "", "u2", 200, `"totalItems":1`)
	f.do(t, "others see none", http.MethodGet, "/api/collections/contents_reports/records", "", "u1", 200, `"totalItems":0`)
	f.do(t, "reporter can't dismiss", http.MethodDelete, "/api/collections/contents_reports/records/"+r.Id, "", "u2", 404)
	f.do(t, "admin dismisses", http.MethodDelete, "/api/collections/contents_reports/records/"+r.Id, "", "admin", 204)
}

func TestOwnersMergeTheirOwnSets(t *testing.T) {
	f := newFixture(t)
	a := f.set(t, "u1")
	f.post(t, "u1", a)
	b := f.set(t, "u1")
	f.post(t, "u1", b)
	shared := f.set(t, "u1")
	f.post(t, "u1", shared)
	f.post(t, "u2", shared)

	merge := func(target string, sources ...string) string {
		return `{"target":"` + target + `","sources":["` + strings.Join(sources, `","`) + `"]}`
	}
	f.do(t, "a source has someone else's post", http.MethodPost, "/api/sets/merge", merge(a, shared), "u1", 403)
	f.do(t, "target isn't mine", http.MethodPost, "/api/sets/merge", merge(a, b), "u2", 403)
	f.do(t, "all mine", http.MethodPost, "/api/sets/merge", merge(a, b), "u1", 200, `"moved":1`)
	if f.record(t, "contents_sets", b) != nil {
		t.Error("merged source should be gone")
	}
	f.do(t, "admin route still admin-only", http.MethodPost, "/api/admin/sets/merge", merge(a, shared), "u1", 403)
	f.do(t, "admin merges anything", http.MethodPost, "/api/sets/merge", merge(a, shared), "admin", 200, `"moved":2`)
}

func TestPropagateSkipsOthersPosts(t *testing.T) {
	f := newFixture(t)
	s := f.set(t, "u1")
	f.post(t, "u1", s)
	f.post(t, "u2", s)

	f.do(t, "co-owner", http.MethodPost, "/api/sets/"+s+"/propagate", `{"fields":["title"]}`, "u1", 200,
		`"updated":1`, `"skipped":1`)
	f.do(t, "admin", http.MethodPost, "/api/sets/"+s+"/propagate", `{"fields":["title"]}`, "admin", 200,
		`"updated":2`, `"skipped":0`)

	lone := f.set(t, "u2")
	f.post(t, "u2", lone)
	f.do(t, "not on the set", http.MethodPost, "/api/sets/"+lone+"/propagate", `{"fields":["title"]}`, "u1", 403)
}

func TestUploaderGuardForAdmins(t *testing.T) {
	f := newFixture(t)
	url := "/api/collections/uploaders/records/" + f.up["u2"]

	f.do(t, "admin blocks import", http.MethodPatch, url, `{"blockIngest":true,"name":"renamed"}`, "admin", 200)
	got := f.record(t, "uploaders", f.up["u2"])
	if !got.GetBool("blockIngest") || got.GetString("name") != "u2" {
		t.Errorf("admin should set only blockIngest, got block=%v name=%q", got.GetBool("blockIngest"), got.GetString("name"))
	}

	f.do(t, "owner can't unblock", http.MethodPatch, url, `{"blockIngest":false,"skipDiscordImport":true}`, "u2", 200)
	got = f.record(t, "uploaders", f.up["u2"])
	if !got.GetBool("blockIngest") || !got.GetBool("skipDiscordImport") {
		t.Errorf("owner kept the block and set their own preference, got block=%v skip=%v",
			got.GetBool("blockIngest"), got.GetBool("skipDiscordImport"))
	}
	f.do(t, "stranger can't touch it", http.MethodPatch, url, `{"name":"x"}`, "u1", 404)
}

// pb_schema.json is the export typegen and reviewers read; the code applies the
// rules at boot. Keep the two saying the same thing.
func TestSchemaExportMatchesAccessRules(t *testing.T) {
	app := newSchemaApp(t)
	for name, rules := range accessRules {
		c, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := map[string]*string{"list": c.ListRule, "view": c.ViewRule, "create": c.CreateRule,
			"update": c.UpdateRule, "delete": c.DeleteRule}
		for kind, want := range rules {
			if got[kind] == nil || *got[kind] != want {
				t.Errorf("pb_schema.json %s.%sRule is out of date with accessRules", name, kind)
			}
		}
	}
}
