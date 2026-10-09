package hooks

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"slices"
	"sync"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// Who may change what, for posts (contents) and sets (contents_sets).
//
// # The model
//
//   - A post belongs to its uploader (contents.uploader, single), and through it
//     to that uploader's account. Only that account or an admin edits or deletes
//     it, and the owner can't re-credit it, re-file it into a stranger's set, or
//     touch the fields the encode pipeline owns.
//   - A set's uploaders are DERIVED: contents_sets.uploader is the distinct
//     uploaders of the posts in it, kept current by the hooks below and never
//     written by clients. Before this, any upload-capable account could append
//     itself to any set's uploader list and so become its co-owner — free to
//     delete the set, which cascades to everyone's posts.
//   - A co-owner (anyone with a post in the set) may edit the set itself. Deleting
//     a set needs every post in it to be yours (or admin); a shared set is left
//     by deleting your own posts instead.
//   - A set that loses its last post through a delete or a move is deleted.
//
// Most of this is plain API rules (ensureAccessRules), which PocketBase checks
// with relation modifiers already resolved (`uploader+` is seen as `uploader`).
// The hooks cover what a rule can't express: keeping set uploaders in sync and
// cleaning up empty sets (model-level, so the bot, scripts and the merge route
// are covered too), which likes a caller may add or remove, and an audit line
// when an admin changes someone else's content.

// RegisterOwnership installs the access rules at boot and the ownership hooks.
func RegisterOwnership(app *pocketbase.PocketBase) {
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureAccessRules(app); err != nil {
			// Non-fatal like the other boot steps, but loud: the rules in the
			// database are then whatever they were before.
			log.Printf("⚠️  ownership: could not apply access rules: %v", err)
		}
		return nil
	})
	bindOwnershipHooks(app)
}

// ─── Access rules ───────────────────────────────────────────────────────────

const (
	ruleAdmin = `@request.auth.isAdmin = true`

	// The fields the encode pipeline, the view counter and provenance own. An
	// owner editing a post never sends them; a request that does is either a bug
	// or someone pointing the post at a file it never had.
	ownerLockedPostFields = `@request.body.file:isset = false && ` +
		`@request.body.original:isset = false && @request.body.preview:isset = false && ` +
		`@request.body.static:isset = false && @request.body.sd:isset = false && ` +
		`@request.body.width:isset = false && @request.body.height:isset = false && ` +
		`@request.body.preview_format:isset = false && @request.body.views:isset = false && ` +
		`@request.body.interpolate:isset = false && @request.body.interpolate_mode:isset = false && ` +
		`@request.body.discord:isset = false && @request.body.mirror:isset = false && ` +
		`@request.body.encodeError:isset = false && @request.body.encodeAttempts:isset = false`

	// Non-owners may only touch what isn't on this list — in practice their own
	// likes and collections (each further checked by a hook).
	nonOwnerLockedPostFields = `@request.body.title:isset = false && @request.body.filename:isset = false && ` +
		`@request.body.file:isset = false && @request.body.original:isset = false && ` +
		`@request.body.preview:isset = false && @request.body.static:isset = false && ` +
		`@request.body.sd:isset = false && @request.body.preview_format:isset = false && ` +
		`@request.body.width:isset = false && @request.body.height:isset = false && ` +
		`@request.body.idol:isset = false && @request.body.group:isset = false && ` +
		`@request.body.tag:isset = false && @request.body.labels:isset = false && ` +
		`@request.body.origin:isset = false && @request.body.filetype:isset = false && ` +
		`@request.body.date:isset = false && @request.body.source:isset = false && ` +
		`@request.body.discord:isset = false && @request.body.mirror:isset = false && ` +
		`@request.body.uploader:isset = false && @request.body.set:isset = false && ` +
		`@request.body.views:isset = false && @request.body.interpolate:isset = false && ` +
		`@request.body.interpolate_mode:isset = false && @request.body.encodeError:isset = false && ` +
		`@request.body.encodeAttempts:isset = false`
)

// accessRules is the source of truth for the rules this file is about. Applied
// at every boot, so the database can't drift from the code (and pb_schema.json
// mirrors it for typegen and review). Rules not listed are left alone.
var accessRules = map[string]map[string]string{
	"contents": {
		// Credited to yourself. Any set is fine here — adding your files to
		// someone else's matching set is the upload page's co-upload feature.
		"create": `@request.auth.canUpload = true && (` + ruleAdmin +
			` || @request.body.uploader.user = @request.auth.id)`,
		"update": `@request.auth.id != "" && (` + ruleAdmin +
			` || (uploader.user = @request.auth.id && ` +
			`@request.body.uploader:changed = false && ` +
			`(@request.body.set:changed = false || @request.body.set = "" || ` +
			`@request.body.set.uploader.user ?= @request.auth.id) && ` +
			ownerLockedPostFields + `)` +
			` || (` + nonOwnerLockedPostFields + `))`,
	},
	"contents_sets": {
		// The creator names only themselves; after that the list is derived.
		"create": `@request.auth.canUpload = true && (` + ruleAdmin +
			` || (@request.body.uploader:length = 1 && @request.body.uploader.user ?= @request.auth.id))`,
		"update": ruleAdmin + ` || (uploader.user ?= @request.auth.id && @request.body.uploader:isset = false)`,
		// Every post in it is yours. Without the `?`, a multi-valued path must
		// match for ALL rows — so one post by anybody else (or by nobody) blocks it.
		"delete": ruleAdmin + ` || (@request.auth.canUpload = true && uploader.user ?= @request.auth.id && ` +
			`contents_via_set.uploader.user = @request.auth.id)`,
	},
	"contents_reports": {
		"list":   `@request.auth.id != '' && (user = @request.auth.id || ` + ruleAdmin + `)`,
		"view":   `@request.auth.id != '' && (user = @request.auth.id || ` + ruleAdmin + `)`,
		"delete": ruleAdmin,
	},
	"uploaders": {
		// Admins may reach any uploader, but RegisterUploaderGuards lets them
		// change only blockIngest on someone else's.
		"update": `user = @request.auth.id || ` + ruleAdmin,
	},
}

func ensureAccessRules(app core.App) error {
	for name, rules := range accessRules {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return fmt.Errorf("find %s: %w", name, err)
		}
		changed := false
		for kind, rule := range rules {
			var target **string
			switch kind {
			case "list":
				target = &collection.ListRule
			case "view":
				target = &collection.ViewRule
			case "create":
				target = &collection.CreateRule
			case "update":
				target = &collection.UpdateRule
			case "delete":
				target = &collection.DeleteRule
			default:
				return fmt.Errorf("%s: unknown rule kind %q", name, kind)
			}
			if *target != nil && **target == rule {
				continue
			}
			r := rule
			*target = &r
			changed = true
		}
		if !changed {
			continue
		}
		if err := app.Save(collection); err != nil {
			return fmt.Errorf("save %s rules: %w", name, err)
		}
		log.Printf("🔐 ownership: applied access rules to %s", name)
	}
	return nil
}

// ─── Hooks ──────────────────────────────────────────────────────────────────

// postPlacement is a post's set and uploader before an update, carried from the
// pre-save hook to the after-success one: once saved, the record only knows
// where it is now, not where it came from.
type postPlacement struct{ set, uploader string }

func bindOwnershipHooks(app core.App) {
	var before sync.Map // record id → postPlacement

	// ── Set uploaders + empty-set cleanup (model level) ──
	//
	// After-success hooks, which inside a transaction run only once it commits:
	// a set being cascade-deleted is gone by then, so the cleanup can't race the
	// cascade, and the merge route's deleted sources are simply not found. The
	// outer app is used for the same reason — the transaction's is closed.

	app.OnRecordAfterCreateSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
		syncSet(app, e.Record.GetString("set"), false)
		return e.Next()
	})

	app.OnRecordUpdate("contents").BindFunc(func(e *core.RecordEvent) error {
		original := e.Record.Original()
		before.Store(e.Record.Id, postPlacement{
			set:      original.GetString("set"),
			uploader: original.GetString("uploader"),
		})
		return e.Next()
	})

	app.OnRecordAfterUpdateSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
		prev, ok := before.LoadAndDelete(e.Record.Id)
		if !ok {
			return e.Next()
		}
		was := prev.(postPlacement)
		set := e.Record.GetString("set")
		switch {
		case was.set != set:
			syncSet(app, was.set, true)
			syncSet(app, set, false)
		case was.uploader != e.Record.GetString("uploader"):
			syncSet(app, set, false)
		}
		return e.Next()
	})

	app.OnRecordAfterUpdateError("contents").BindFunc(func(e *core.RecordErrorEvent) error {
		before.Delete(e.Record.Id)
		return e.Next()
	})

	app.OnRecordAfterDeleteSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
		syncSet(app, e.Record.GetString("set"), true)
		return e.Next()
	})

	// ── Likes (request level) ──
	app.OnRecordCreateRequest("contents").BindFunc(guardLikes)
	app.OnRecordUpdateRequest("contents").BindFunc(guardLikes)

	// ── Audit (request level) ──
	app.OnRecordDeleteRequest("contents", "contents_sets").BindFunc(auditAdminWrite("deleted"))
	app.OnRecordUpdateRequest("contents").BindFunc(auditAdminWrite("edited"))
}

// syncSet makes the set's uploader list the distinct uploaders of its posts, and
// deletes the set when it has none left and dropEmpty is set (a post was deleted
// or moved out — never on create, when a fresh set is legitimately empty while
// its files are still uploading).
//
// Best effort: a failure here leaves the list stale or the set empty, both
// visible and harmless, so it logs rather than failing the write that caused it.
func syncSet(app core.App, setID string, dropEmpty bool) {
	if setID == "" {
		return
	}
	set, err := app.FindRecordById("contents_sets", setID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Printf("⚠️  ownership: load set %s: %v", setID, err)
		}
		return // deleted with its posts, or by the merge that moved them
	}

	var rows []struct {
		Uploader string `db:"uploader"`
		Posts    int    `db:"posts"`
	}
	err = app.DB().NewQuery(
		"SELECT COALESCE([[uploader]], '') AS uploader, COUNT(*) AS posts FROM {{contents}} " +
			"WHERE [[set]] = {:set} GROUP BY 1",
	).Bind(dbx.Params{"set": setID}).All(&rows)
	if err != nil {
		log.Printf("⚠️  ownership: count posts in set %s: %v", setID, err)
		return
	}

	if len(rows) == 0 {
		if !dropEmpty {
			return
		}
		if err := app.Delete(set); err != nil && !errors.Is(err, sql.ErrNoRows) {
			log.Printf("⚠️  ownership: delete empty set %s: %v", setID, err)
		}
		return
	}

	present := map[string]bool{}
	for _, r := range rows {
		if r.Uploader != "" {
			present[r.Uploader] = true
		}
	}
	// Keep the existing order for those still there (the first is the one
	// shown first), then append newcomers.
	var want []string
	for _, id := range set.GetStringSlice("uploader") {
		if present[id] {
			want = append(want, id)
			delete(present, id)
		}
	}
	newcomers := make([]string, 0, len(present))
	for id := range present {
		newcomers = append(newcomers, id)
	}
	slices.Sort(newcomers)
	want = append(want, newcomers...)

	if slices.Equal(want, set.GetStringSlice("uploader")) {
		return
	}
	set.Set("uploader", want)
	if err := app.Save(set); err != nil {
		log.Printf("⚠️  ownership: update uploaders of set %s: %v", setID, err)
	}
}

// guardLikes lets a caller add or remove only their own likes.
//
// contents.likes is a back-reference to users_likes rows, written by the client
// alongside the row itself (useLikeApi). The update rule has to leave `likes`
// open to non-owners — that's how anyone likes a post — so without this anyone
// could also strip every like off a post, or attach likes that aren't theirs.
func guardLikes(e *core.RecordRequestEvent) error {
	if e.HasSuperuserAuth() || e.Auth == nil || e.Auth.GetBool("isAdmin") {
		return e.Next()
	}
	prev := e.Record.Original().GetStringSlice("likes")
	next := e.Record.GetStringSlice("likes")

	for _, id := range addedStrings(prev, next) {
		like, err := e.App.FindRecordById("users_likes", id)
		if err != nil || like.GetString("user") != e.Auth.Id || like.GetString("content") != e.Record.Id {
			return e.ForbiddenError("You can only add your own likes.", nil)
		}
	}
	for _, id := range addedStrings(next, prev) {
		like, err := e.App.FindRecordById("users_likes", id)
		if err != nil {
			continue // already deleted — that's the normal unlike order
		}
		if like.GetString("user") != e.Auth.Id {
			return e.ForbiddenError("You can only remove your own likes.", nil)
		}
	}
	return e.Next()
}

// auditAdminWrite logs an admin deleting or editing a post or set that isn't
// theirs, once the write has succeeded. system_logs is otherwise for problems;
// these are the one kind of routine event worth being able to look back on.
func auditAdminWrite(verb string) func(e *core.RecordRequestEvent) error {
	return func(e *core.RecordRequestEvent) error {
		if e.Auth == nil || !e.Auth.GetBool("isAdmin") || e.HasSuperuserAuth() {
			return e.Next()
		}
		own := callerOwnsRecord(e.App, e.Record, e.Auth.Id)
		snapshot := map[string]any{
			"admin":      e.Auth.Id,
			"collection": e.Collection.Name,
			"record":     e.Record.Id,
			"title":      e.Record.Original().GetString("title"),
		}
		if err := e.Next(); err != nil {
			return err
		}
		if !own {
			kind := "post"
			if e.Collection.Name == "contents_sets" {
				kind = "set"
			}
			LogServerInfo(fmt.Sprintf("Admin %s someone else's %s", verb, kind), snapshot)
		}
		return nil
	}
}

// callerUploaderIDs is the user's uploader record(s) — one in practice (unique
// index on uploaders.user), but a list costs nothing and survives that changing.
func callerUploaderIDs(app core.App, userID string) []string {
	records, err := app.FindAllRecords("uploaders", dbx.HashExp{"user": userID})
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.Id)
	}
	return ids
}

// callerOwnsRecord: a post is yours when its uploader is; a set when one of its
// (derived) uploaders is.
func callerOwnsRecord(app core.App, record *core.Record, userID string) bool {
	mine := callerUploaderIDs(app, userID)
	for _, id := range record.Original().GetStringSlice("uploader") {
		if slices.Contains(mine, id) {
			return true
		}
	}
	return false
}
