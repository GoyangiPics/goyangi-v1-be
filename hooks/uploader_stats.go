package hooks

import (
	"fmt"
	"log"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// The uploaders directory needs an upload count per uploader.
//
// It used to get one by asking: a getFullList of `uploaders`, then one
// `contents` count query PER uploader, all fired at once from Promise.all.
// That was ~30 requests when it was written. At 400+ uploaders it is 400+
// concurrent requests, each a `uploader.id ?= ...` join that walks every
// content row because nothing indexed `contents.uploader` — a single open of
// /uploaders queued hundreds of full scans on SQLite's one writer/few readers
// and stalled the whole site behind them.
//
// Two pieces here, both ensured at boot for the reason ensureDimensionFields
// documents (no migrations directory; a deploy must not depend on someone
// remembering to click through the admin UI first):
//
//   - `uploaders_stats`, a view collection: every uploader with its upload
//     count, so the page is ONE request. Same pattern as `labels_stats`.
//   - an index on `contents (uploader, filetype)`, which is what makes that
//     view a per-uploader index walk rather than a scan of the whole table.
//     It also serves the profile page's `uploader.name = ...` listing and
//     count, which joined the same unindexed column.
//
// pb_schema.json carries both. It is a hand-maintained export; leaving it
// behind would mean the next `pnpm typegen` in the frontend dropped the view's
// types.

const (
	uploadersStatsCollection = "uploaders_stats"
	// A fixed id so the record created here matches the pb_schema.json export
	// (labels_stats does the same with pbc_1750000004).
	uploadersStatsCollectionID = "pbc_1750000005"

	contentsUploaderIndexName = "idx_contents_uploader_filetype"
	contentsUploaderIndex     = "CREATE INDEX `" + contentsUploaderIndexName +
		"` ON `contents` (`uploader`, `filetype`)"
)

// uploadersStatsViewQuery mirrors the count the profile page shows, which is
// `uploader.name = X && filetype != "sticker"`: stickers are excluded from the
// join so they never reach COUNT.
//
// Column notes, because PocketBase types a view's fields from its SELECT list
// (core/view.go parseQueryToFields):
//   - `u.id AS id` is the view's primary key;
//   - `u.user AS user` clones the relation field, so `expand=user` keeps
//     working and the page can show avatars and spot account-less profiles;
//   - `COUNT(...)` becomes an integer number field;
//   - everything after a join's ON is ignored by the parser, so the filtered
//     join is safe.
const uploadersStatsViewQuery = "SELECT u.id AS id, u.name AS name, u.aliases AS aliases, " +
	"u.user AS user, u.created AS created, COUNT(c.id) AS uploads " +
	"FROM uploaders u " +
	"LEFT JOIN contents c ON c.uploader = u.id AND c.filetype != 'sticker' " +
	"GROUP BY u.id, u.name, u.aliases, u.user, u.created"

// RegisterUploaderStats ensures the `uploaders_stats` view and the
// `contents (uploader, filetype)` index exist.
//
// After e.Next() and non-fatal, like RegisterUploaderFields: collections can
// only be queried once the DB is up, and a failure here must not stop the app
// serving — the site works without either, it is just slow on /uploaders.
func RegisterUploaderStats(app *pocketbase.PocketBase) {
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		// The view selects `u.aliases`, which ensureUploaderFields adds. It
		// cannot be left to registration order: code after e.Next() runs in
		// REVERSE order of registration (each handler's body runs as the chain
		// unwinds), so RegisterUploaderFields being registered first means its
		// body runs AFTER this one. Calling it here is idempotent and makes
		// the dependency explicit instead of a property of main.go's layout.
		if err := ensureUploaderFields(app); err != nil {
			log.Printf("⚠️  uploaders: could not ensure uploader fields: %v", err)
		}
		// Index first: the view's first query is the one that benefits.
		if err := ensureContentsUploaderIndex(app); err != nil {
			log.Printf("⚠️  uploaders: could not ensure contents uploader index: %v", err)
		}
		if err := ensureUploadersStatsView(app); err != nil {
			log.Printf("⚠️  uploaders: could not ensure %s view: %v", uploadersStatsCollection, err)
		}
		return nil
	})
}

// ensureContentsUploaderIndex adds the (uploader, filetype) index to
// `contents` if no index of that name exists. Idempotent.
func ensureContentsUploaderIndex(app core.App) error {
	collection, err := app.FindCollectionByNameOrId("contents")
	if err != nil {
		return fmt.Errorf("find contents: %w", err)
	}
	for _, idx := range collection.Indexes {
		if strings.Contains(idx, "`"+contentsUploaderIndexName+"`") {
			return nil
		}
	}
	collection.Indexes = append(collection.Indexes, contentsUploaderIndex)
	// Saving a collection with a changed Indexes list is what creates the
	// index; PocketBase diffs old vs new and runs the CREATE INDEX itself.
	return app.Save(collection)
}

// ensureUploadersStatsView creates the view, or brings an existing one's
// query back in line with uploadersStatsViewQuery so this file stays the
// source of truth for what the view computes. Idempotent.
func ensureUploadersStatsView(app core.App) error {
	existing, err := app.FindCollectionByNameOrId(uploadersStatsCollection)
	if err == nil {
		if !existing.IsView() {
			return fmt.Errorf("%s exists but is not a view", uploadersStatsCollection)
		}
		if existing.ViewQuery == uploadersStatsViewQuery {
			return nil
		}
		existing.ViewQuery = uploadersStatsViewQuery
		return app.Save(existing)
	}

	view := core.NewViewCollection(uploadersStatsCollection, uploadersStatsCollectionID)
	view.ViewQuery = uploadersStatsViewQuery
	// Public, matching `uploaders` itself: the directory shows the same names
	// to everyone, and a count adds nothing an unauthenticated caller could not
	// already compute from the public `contents` list.
	view.ListRule = types.Pointer("")
	view.ViewRule = types.Pointer("")
	return app.Save(view)
}
