package hooks

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// collection makes a public collection owned by `owner`.
func (f *fixture) collection(t *testing.T, owner string) string {
	t.Helper()
	col, _ := f.app.FindCollectionByNameOrId("contents_collections")
	c := core.NewRecord(col)
	c.Set("title", "faves")
	c.Set("isPublic", true)
	c.Set("user", []string{f.users[owner].Id})
	mustSave(t, f.app, c)
	return c.Id
}

// stamped makes a post with an explicit `created`, so "newest" is not left to
// clock resolution.
func (f *fixture) stamped(t *testing.T, at time.Time) *core.Record {
	t.Helper()
	p := f.record(t, "contents", f.post(t, "u1", ""))
	created, _ := types.ParseDateTime(at)
	p.SetRaw("created", created)
	if err := f.app.SaveNoValidate(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// setCollections rewrites a post's membership from a freshly loaded copy, the
// way a request would: the hooks diff against Original(), which a reused record
// would still hold from before its last save.
func setCollections(t *testing.T, f *fixture, p *core.Record, ids ...string) {
	t.Helper()
	fresh := f.record(t, "contents", p.Id)
	fresh.Set("collections", ids)
	if err := f.app.SaveNoValidate(fresh); err != nil {
		t.Fatal(err)
	}
}

func previewsOf(t *testing.T, f *fixture, id string) ([]string, int) {
	t.Helper()
	c := f.record(t, "contents_collections", id)
	return c.GetStringSlice("previews"), c.GetInt("contentCount")
}

func TestBackfillCountsAndNewestPreviews(t *testing.T) {
	f := newFixture(t)
	col := f.collection(t, "u1")
	empty := f.collection(t, "u1")

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var posts []*core.Record
	for i := range 6 {
		p := f.stamped(t, base.Add(time.Duration(i)*time.Hour))
		setCollections(t, f, p, col)
		posts = append(posts, p)
	}

	if _, err := backfillCollectionPreviews(f.app); err != nil {
		t.Fatal(err)
	}
	previews, count := previewsOf(t, f, col)
	if count != 6 {
		t.Errorf("count = %d, want 6", count)
	}
	want := []string{posts[5].Id, posts[4].Id, posts[3].Id, posts[2].Id}
	if !slices.Equal(previews, want) {
		t.Errorf("previews = %v, want the newest four %v", previews, want)
	}
	if previews, count := previewsOf(t, f, empty); count != 0 || len(previews) != 0 {
		t.Errorf("empty collection = %v/%d, want none", previews, count)
	}
}

func TestMembershipKeepsPreviewsCurrent(t *testing.T) {
	f := newFixture(t)
	bindCollectionPreviewHooks(f.app)
	col := f.collection(t, "u1")

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var posts []*core.Record
	for i := range 5 {
		p := f.stamped(t, base.Add(time.Duration(i)*time.Hour))
		setCollections(t, f, p, col)
		posts = append(posts, p)
	}
	previews, count := previewsOf(t, f, col)
	if count != 5 {
		t.Errorf("after adds: count = %d, want 5", count)
	}
	if want := []string{posts[4].Id, posts[3].Id, posts[2].Id, posts[1].Id}; !slices.Equal(previews, want) {
		t.Errorf("after adds: previews = %v, want newest-added first %v", previews, want)
	}

	// A post that isn't a thumbnail leaves: count only.
	setCollections(t, f, posts[0])
	if previews, count = previewsOf(t, f, col); count != 4 || len(previews) != 4 {
		t.Errorf("non-preview leave: %v/%d, want 4 thumbnails, count 4", previews, count)
	}

	// A thumbnail leaves: rebuilt from what's still in.
	setCollections(t, f, posts[4])
	previews, count = previewsOf(t, f, col)
	if count != 3 || slices.Contains(previews, posts[4].Id) || len(previews) != 3 {
		t.Errorf("preview leave: %v/%d, want the other three", previews, count)
	}

	// A thumbnail is deleted outright.
	if err := f.app.Delete(f.record(t, "contents", posts[3].Id)); err != nil {
		t.Fatal(err)
	}
	previews, count = previewsOf(t, f, col)
	if want := []string{posts[2].Id, posts[1].Id}; count != 2 || !slices.Equal(previews, want) {
		t.Errorf("after delete: %v/%d, want %v/2", previews, count, want)
	}
}

func TestPreviewFieldsAreNotClientWritable(t *testing.T) {
	f := newFixture(t)
	bindCollectionPreviewHooks(f.app)
	col := f.collection(t, "u1")
	p := f.post(t, "u1", "")

	f.do(t, "owner sets previews", http.MethodPatch,
		"/api/collections/contents_collections/records/"+col,
		`{"previews":["`+p+`"],"contentCount":99,"title":"renamed"}`, "u1", 200, `"title":"renamed"`)
	if previews, count := previewsOf(t, f, col); count != 0 || len(previews) != 0 {
		t.Errorf("derived fields took a client write: %v/%d", previews, count)
	}
}

// The production boot path: a server whose schema predates the fields.
func TestEnsureAddsAndFillsFields(t *testing.T) {
	f := newFixture(t)
	col, _ := f.app.FindCollectionByNameOrId("contents_collections")
	col.Fields.RemoveByName("previews")
	col.Fields.RemoveByName("contentCount")
	if err := f.app.Save(col); err != nil {
		t.Fatal(err)
	}
	id := f.collection(t, "u1")
	setCollections(t, f, f.record(t, "contents", f.post(t, "u1", "")), id)

	if err := ensureCollectionPreviewFields(f.app); err != nil {
		t.Fatal(err)
	}
	if previews, count := previewsOf(t, f, id); count != 1 || len(previews) != 1 {
		t.Errorf("after ensure: %v/%d, want one post", previews, count)
	}
	// Idempotent: a second boot finds the fields and leaves them be.
	if err := ensureCollectionPreviewFields(f.app); err != nil {
		t.Fatal(err)
	}
}
