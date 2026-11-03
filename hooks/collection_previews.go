package hooks

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// Collection previews: the newest few posts and a post count, stored on the
// collection itself.
//
// The public /collections page used to learn both through the back-relation
// `contents_via_collections` — the emptiness filter (`?!= ""`) and the
// thumbnail expand. `contents.collections` is a multi relation, which
// PocketBase stores as a JSON array per row, so every such lookup unpacked the
// array of all ~245k contents; no index can see inside JSON. The listing took
// ~5 s, of which ~0.2 s was the collections themselves, and the expand shipped
// every post of every collection to render four thumbnails each.
//
// The same shape `users_likes` was the answer to for likes, but a join
// collection for collections would mean moving every membership write in the
// site. These two fields are the smaller move: the listing filters on
// `contentCount > 0` and expands `previews`, both plain columns on a table of a
// few dozen rows.
//
// Kept current from the contents model hooks, after each write commits, so
// every path that changes membership — the site's `collections+` updates, bulk
// adds, deletes, merges — is covered without the frontend knowing.

const (
	// collectionPreviewCount is how many thumbnails a collection card shows.
	collectionPreviewCount = 4
)

func RegisterCollectionPreviews(app *pocketbase.PocketBase) {
	// After e.Next() and non-fatal, like RegisterUploaderFields: the fields need
	// the DB up, and a failure must not stop the app serving.
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureCollectionPreviewFields(app); err != nil {
			log.Printf("⚠️  collections: could not ensure preview fields: %v", err)
		}
		return nil
	})
	bindCollectionPreviewHooks(app)
}

// ensureCollectionPreviewFields adds `previews` and `contentCount` to
// contents_collections and fills them, in one transaction.
//
// One transaction so a failed backfill also rolls the fields back: the next
// boot then sees them missing and tries again, rather than serving collections
// whose zero counts hide them from the listing.
//
// pb_schema.json carries the same fields, for the reason ensureUploaderFields
// gives.
func ensureCollectionPreviewFields(app core.App) error {
	collection, err := app.FindCollectionByNameOrId("contents_collections")
	if err != nil {
		return fmt.Errorf("find contents_collections: %w", err)
	}
	if collection.Fields.GetByName("previews") != nil && collection.Fields.GetByName("contentCount") != nil {
		return nil
	}
	contents, err := app.FindCollectionByNameOrId("contents")
	if err != nil {
		return fmt.Errorf("find contents: %w", err)
	}

	return app.RunInTransaction(func(txApp core.App) error {
		if collection.Fields.GetByName("previews") == nil {
			collection.Fields.Add(&core.RelationField{
				Name:         "previews",
				CollectionId: contents.Id,
				MaxSelect:    collectionPreviewCount,
			})
		}
		if collection.Fields.GetByName("contentCount") == nil {
			collection.Fields.Add(&core.NumberField{Name: "contentCount", OnlyInt: true})
		}
		if err := txApp.Save(collection); err != nil {
			return fmt.Errorf("save contents_collections: %w", err)
		}
		n, err := backfillCollectionPreviews(txApp)
		if err != nil {
			return fmt.Errorf("backfill: %w", err)
		}
		log.Printf("🖼️  collections: added preview fields and filled %d collection(s)", n)
		return nil
	})
}

// membershipJSON is a contents row's `collections` array, safe to feed to
// json_each even if a row holds something other than a JSON array.
const membershipJSON = "CASE WHEN json_valid(c.collections) THEN c.collections ELSE '[]' END"

// backfillCollectionPreviews recomputes both fields for every collection.
//
// The one full scan of contents' membership arrays this feature ever does:
// counts and the newest four per collection in a single pass each. Raw SQL for
// the reasons backfillOrigin gives — no hooks, no `updated` bump.
func backfillCollectionPreviews(app core.App) (int, error) {
	type countRow struct {
		Collection string `db:"collection"`
		N          int    `db:"n"`
	}
	var counts []countRow
	err := app.DB().NewQuery(
		"SELECT je.value AS collection, COUNT(*) AS n FROM {{contents}} c, json_each(" + membershipJSON + ") je" +
			" GROUP BY je.value",
	).All(&counts)
	if err != nil {
		return 0, fmt.Errorf("count: %w", err)
	}

	type previewRow struct {
		Collection string `db:"collection"`
		Content    string `db:"content"`
	}
	var previews []previewRow
	err = app.DB().NewQuery(
		"SELECT collection, content FROM (" +
			"SELECT je.value AS collection, c.id AS content," +
			" ROW_NUMBER() OVER (PARTITION BY je.value ORDER BY c.created DESC) AS rn" +
			" FROM {{contents}} c, json_each(" + membershipJSON + ") je" +
			") WHERE rn <= {:limit} ORDER BY collection, rn",
	).Bind(dbx.Params{"limit": collectionPreviewCount}).All(&previews)
	if err != nil {
		return 0, fmt.Errorf("previews: %w", err)
	}

	byCollection := map[string][]string{}
	for _, p := range previews {
		byCollection[p.Collection] = append(byCollection[p.Collection], p.Content)
	}
	countOf := map[string]int{}
	for _, c := range counts {
		countOf[c.Collection] = c.N
	}

	var ids []string
	if err := app.DB().NewQuery("SELECT id FROM {{contents_collections}}").Column(&ids); err != nil {
		return 0, fmt.Errorf("list collections: %w", err)
	}
	for _, id := range ids {
		list := byCollection[id]
		if list == nil {
			list = []string{}
		}
		encoded, err := json.Marshal(list)
		if err != nil {
			return 0, err
		}
		_, err = app.DB().NewQuery(
			"UPDATE {{contents_collections}} SET [[previews]] = {:previews}, [[contentCount]] = {:count} WHERE id = {:id}",
		).Bind(dbx.Params{"previews": string(encoded), "count": countOf[id], "id": id}).Execute()
		if err != nil {
			return 0, fmt.Errorf("update %s: %w", id, err)
		}
	}
	return len(ids), nil
}

// bindCollectionPreviewHooks is the request and model half, split out so tests
// can bind it on a test app without the bootstrap step.
func bindCollectionPreviewHooks(app core.App) {
	// Derived fields: nobody writes them through the API, owners included —
	// an owner could otherwise point their collection's thumbnails at anything.
	guard := func(e *core.RecordRequestEvent) error {
		if !e.HasSuperuserAuth() {
			original := e.Record.Original()
			e.Record.Set("previews", original.Get("previews"))
			e.Record.Set("contentCount", original.Get("contentCount"))
		}
		return e.Next()
	}
	app.OnRecordCreateRequest("contents_collections").BindFunc(guard)
	app.OnRecordUpdateRequest("contents_collections").BindFunc(guard)

	app.OnRecordAfterCreateSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		syncCollectionMembership(e.App, e.Record.Id, e.Record.GetStringSlice("collections"), nil)
		return nil
	})
	app.OnRecordAfterUpdateSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		prev := e.Record.Original().GetStringSlice("collections")
		next := e.Record.GetStringSlice("collections")
		syncCollectionMembership(e.App, e.Record.Id, addedStrings(prev, next), addedStrings(next, prev))
		return nil
	})
	app.OnRecordAfterDeleteSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		syncCollectionMembership(e.App, e.Record.Id, nil, e.Record.GetStringSlice("collections"))
		return nil
	})
}

// syncCollectionMembership applies one post's joins and leaves to the
// collections' derived fields. Logged rather than returned: the post's own
// write has already committed, and a collection whose preview lags is a far
// smaller problem than a save that reports failure after succeeding.
func syncCollectionMembership(app core.App, contentID string, joined, left []string) {
	for _, id := range joined {
		if err := updateCollectionPreview(app, id, contentID, true); err != nil {
			log.Printf("⚠️  collections: %s joining %s: %v", contentID, id, err)
		}
	}
	for _, id := range left {
		if err := updateCollectionPreview(app, id, contentID, false); err != nil {
			log.Printf("⚠️  collections: %s leaving %s: %v", contentID, id, err)
		}
	}
}

// updateCollectionPreview is a read-modify-write of one collection, inside a
// transaction. PocketBase writes through a single connection, so the
// transaction also serialises this against a bulk add's other updates to the
// same collection — none can read the count between another's read and write.
//
// A join puts the post first (newest added leads) and counts it. A leave
// uncounts it, and rebuilds the thumbnails only when they come up short — the
// post was one of them, or there are fewer than the count allows. The second
// case is a delete: PocketBase strips a deleted post's id out of `previews`
// itself, before this runs, so "was it a thumbnail" can't be asked by then.
// The rebuild is the one place a membership scan is still needed, and most
// removals never reach it.
func updateCollectionPreview(app core.App, collectionID, contentID string, joined bool) error {
	return app.RunInTransaction(func(txApp core.App) error {
		record, err := txApp.FindRecordById("contents_collections", collectionID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil // the collection itself was deleted
			}
			return err
		}
		previews := record.GetStringSlice("previews")
		count := record.GetInt("contentCount")

		if joined {
			count++
			previews = slices.DeleteFunc(previews, func(id string) bool { return id == contentID })
			previews = append([]string{contentID}, previews...)
			if len(previews) > collectionPreviewCount {
				previews = previews[:collectionPreviewCount]
			}
		} else {
			count = max(0, count-1)
			if slices.Contains(previews, contentID) || len(previews) < min(count, collectionPreviewCount) {
				previews, err = newestInCollection(txApp, collectionID, contentID)
				if err != nil {
					return err
				}
			}
		}

		record.Set("previews", previews)
		record.Set("contentCount", count)
		return txApp.SaveNoValidate(record)
	})
}

// newestInCollection is the newest few posts in a collection other than
// `except`, the post leaving it. Its write has committed by the time this runs,
// so the scan shouldn't find it anyway; excluding it costs nothing and keeps a
// leaving post out of the thumbnails whatever the timing.
func newestInCollection(app core.App, collectionID, except string) ([]string, error) {
	var ids []string
	err := app.DB().NewQuery(
		"SELECT c.id FROM {{contents}} c, json_each(" + membershipJSON + ") je" +
			" WHERE je.value = {:collection} AND c.id != {:except}" +
			" ORDER BY c.created DESC LIMIT {:limit}",
	).Bind(dbx.Params{"collection": collectionID, "except": except, "limit": collectionPreviewCount}).Column(&ids)
	if ids == nil {
		ids = []string{}
	}
	return ids, err
}
