package hooks

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

const (
	// maxMergeSources bounds one merge. The whole thing is a single transaction
	// holding SQLite's write lock, and the encode pipeline writes on the same
	// connection — a runaway merge would stall uploads.
	maxMergeSources = 20

	// maxMergeChildren / maxPropagateChildren bound how many contents one call
	// will touch. Exceeding it is an error rather than a silent truncation.
	maxMergeChildren     = 2000
	maxPropagateChildren = 2000
)

var (
	errSetNotFound      = errors.New("sets: no such set")
	errSetSourceOverlap = errors.New("sets: target cannot also be a source")
	errSetTooManyKids   = errors.New("sets: too many child records")
	errSetRelOverflow   = errors.New("sets: merged relation would exceed the field limit")
	errSetStillHasKids  = errors.New("sets: source still has children")
	errSetForbidden     = errors.New("sets: not permitted")
)

// RegisterSetRoutes adds the set-management endpoints.
//
//	POST /api/admin/sets/merge      {target, sources[], mergeRelations} (admin)
//	POST /api/sets/{id}/propagate   {fields[]}                         (set uploader or admin)
func RegisterSetRoutes(app *pocketbase.PocketBase) {
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.POST("/api/admin/sets/merge", mergeSets(app)).
			Bind(apis.RequireAuth("users"), requireAdmin())

		// NOT admin-gated: the normal case is an uploader fixing their own set.
		// Authorisation is checked inside, against the set's uploader list.
		e.Router.POST("/api/sets/{id}/propagate", propagateSet(app)).
			Bind(apis.RequireAuth("users"))

		return e.Next()
	})
}

// ─── Merge ──────────────────────────────────────────────────────────────────

type mergeSetsBody struct {
	Target  string   `json:"target"`
	Sources []string `json:"sources"`
	// MergeRelations unions the sources' idol/group/uploader into the target.
	// Defaults to true; send false to keep the target's metadata untouched.
	MergeRelations *bool `json:"mergeRelations"`
}

// mergeSets moves every source set's children onto the target, unions their
// relation arrays, and deletes the emptied sources.
//
// # The hazard
//
// contents.set is cascadeDelete, and PocketBase runs cascade deletions through
// the full ORM — so deleting a source set BEFORE its children have been
// reassigned destroys the children AND fires the R2 cleanup hook on each one,
// taking their stored objects with them.
//
// Two things make this safe rather than merely careful:
//
//  1. Order. Children move first, relations merge, sources are deleted last, all
//     in one transaction. Never any other order.
//  2. A count guard immediately before each delete, inside the transaction. If a
//     concurrent upload landed a child after the move, or a reassign was somehow
//     skipped, this turns silent data loss into a 409.
//
// Rollback is genuinely safe: inside a transaction PocketBase defers
// OnRecord*AfterSuccess hooks to commit and skips them when the transaction
// errored, so a failed merge deletes zero R2 objects.
func mergeSets(app *pocketbase.PocketBase) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		var body mergeSetsBody
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Invalid request body.", err)
		}
		if body.Target == "" || len(body.Sources) == 0 {
			return e.BadRequestError("A target and at least one source are required.", nil)
		}
		if len(body.Sources) > maxMergeSources {
			return e.BadRequestError(
				fmt.Sprintf("At most %d source sets can be merged at once.", maxMergeSources), nil)
		}

		mergeRelations := true
		if body.MergeRelations != nil {
			mergeRelations = *body.MergeRelations
		}

		var (
			moved   int
			deleted []string
			target  *core.Record
		)

		txErr := app.RunInTransaction(func(txApp core.App) error {
			var err error
			target, err = txApp.FindRecordById("contents_sets", body.Target)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errSetNotFound
				}
				return err
			}

			// Load every source up front so a bad id fails before any write.
			seen := map[string]struct{}{body.Target: {}}
			sources := make([]*core.Record, 0, len(body.Sources))
			for _, id := range body.Sources {
				if _, dup := seen[id]; dup {
					if id == body.Target {
						return errSetSourceOverlap
					}
					continue // duplicate source id, harmless
				}
				seen[id] = struct{}{}
				src, err := txApp.FindRecordById("contents_sets", id)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return errSetNotFound
					}
					return err
				}
				sources = append(sources, src)
			}

			// 1. Children first. Always.
			for _, src := range sources {
				children, err := txApp.FindRecordsByFilter(
					"contents", "set={:id}", "created", maxMergeChildren, 0,
					dbx.Params{"id": src.Id},
				)
				if err != nil {
					return err
				}
				if len(children) >= maxMergeChildren {
					return errSetTooManyKids
				}
				for _, child := range children {
					child.Set("set", target.Id)
					// Fires OnRecordUpdate("contents") but NOT OnRecordCreate, so
					// the id-generating hook doesn't run and children keep their
					// ids. Also fires the R2 update-cleanup hook with an empty URL
					// diff, which is a no-op.
					if err := txApp.Save(child); err != nil {
						return err
					}
					moved++
				}
			}

			// 2. Relations.
			if mergeRelations {
				for _, field := range []string{"idol", "group", "uploader"} {
					union := target.GetStringSlice(field)
					for _, src := range sources {
						union = unionStrings(union, src.GetStringSlice(field)...)
					}
					// maxSelect on these fields is 999.
					if len(union) > 999 {
						return errSetRelOverflow
					}
					target.Set(field, union)
				}
				if err := txApp.Save(target); err != nil {
					return err
				}
			}

			// 3. Sources last, each guarded.
			for _, src := range sources {
				var remaining int
				if err := txApp.DB().NewQuery(
					"SELECT COUNT(*) FROM `contents` WHERE `set` = {:id}",
				).Bind(dbx.Params{"id": src.Id}).Row(&remaining); err != nil {
					return err
				}
				if remaining != 0 {
					return fmt.Errorf("%w: %s has %d", errSetStillHasKids, src.Id, remaining)
				}
				if err := txApp.Delete(src); err != nil {
					return err
				}
				deleted = append(deleted, src.Id)
			}

			return nil
		})

		switch {
		case errors.Is(txErr, errSetNotFound):
			return e.NotFoundError("One of the sets does not exist.", nil)
		case errors.Is(txErr, errSetSourceOverlap):
			return e.BadRequestError("The target set cannot also be a source.", nil)
		case errors.Is(txErr, errSetTooManyKids):
			return e.BadRequestError(
				fmt.Sprintf("A source set has more than %d items — merge it manually.", maxMergeChildren), nil)
		case errors.Is(txErr, errSetRelOverflow):
			return e.BadRequestError("The merged set would exceed the idol/group/uploader limit.", nil)
		case errors.Is(txErr, errSetStillHasKids):
			// 409, not 500: nothing was lost, the guard did its job.
			return apis.NewApiError(409, "A source set gained items during the merge. Nothing was changed — try again.", nil)
		case txErr != nil:
			return e.InternalServerError("Could not merge the sets.", txErr)
		}

		return e.JSON(200, map[string]any{
			"target":   target.Id,
			"moved":    moved,
			"deleted":  deleted,
			"idol":     target.GetStringSlice("idol"),
			"group":    target.GetStringSlice("group"),
			"uploader": target.GetStringSlice("uploader"),
		})
	}
}

// unionStrings appends the values not already present, preserving order.
func unionStrings(base []string, add ...string) []string {
	seen := make(map[string]struct{}, len(base)+len(add))
	for _, v := range base {
		seen[v] = struct{}{}
	}
	for _, v := range add {
		if v == "" {
			continue
		}
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		base = append(base, v)
	}
	return base
}

// ─── Propagate ──────────────────────────────────────────────────────────────

type propagateSetBody struct {
	Fields []string `json:"fields"`
}

// propagatableSetFields are the fields a set can push down to its children.
//
// Deliberately excludes everything the pipeline owns (file, original, preview,
// static, sd), everything provenance-related (discord, mirror, origin), the
// counters (views, likes) and the `set` link itself.
//
// `tag` is absent because contents_sets has no tag field and does not need one —
// the set-list tag filter already resolves through contents_via_set.tag. Adding a
// set-level tag purely to enable propagation would be the wrong fix.
var propagatableSetFields = map[string]bool{
	"title":    true,
	"idol":     true,
	"group":    true,
	"date":     true,
	"uploader": true,
}

// propagateSet copies chosen fields from a set down onto all of its children.
//
// A backend endpoint rather than a client-side batch, because a batch cannot
// work: contents.updateRule requires uploader.user = @request.auth.id to change
// title/idol/group/date, and a set legitimately holds clips from several
// uploaders (uploads.vue appends co-uploaders when adding to an existing set).
// So a browser batch 403s on every clip the editor didn't upload — and
// PocketBase batches are atomic, meaning one rejection fails all of them and the
// user gets nothing.
func propagateSet(app *pocketbase.PocketBase) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		setID := e.Request.PathValue("id")
		if setID == "" {
			return e.BadRequestError("A set id is required.", nil)
		}

		var body propagateSetBody
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Invalid request body.", err)
		}

		fields := make([]string, 0, len(body.Fields))
		for _, f := range body.Fields {
			if !propagatableSetFields[f] {
				return e.BadRequestError(fmt.Sprintf("Field %q cannot be propagated.", f), nil)
			}
			fields = append(fields, f)
		}
		if len(fields) == 0 {
			return e.BadRequestError("At least one field to propagate is required.", nil)
		}

		var updated int

		txErr := app.RunInTransaction(func(txApp core.App) error {
			set, err := txApp.FindRecordById("contents_sets", setID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errSetNotFound
				}
				return err
			}

			if !e.Auth.GetBool("isAdmin") {
				allowed, err := callerOwnsSet(txApp, set, e.Auth.Id)
				if err != nil {
					return err
				}
				if !allowed {
					return errSetForbidden
				}
			}

			children, err := txApp.FindRecordsByFilter(
				"contents", "set={:id}", "created", maxPropagateChildren, 0,
				dbx.Params{"id": setID},
			)
			if err != nil {
				return err
			}
			if len(children) >= maxPropagateChildren {
				return errSetTooManyKids
			}

			for _, child := range children {
				for _, field := range fields {
					switch field {
					case "title":
						// Set titles carry a "YYMMDD " prefix that content titles
						// don't (see createSetRecord) — strip it.
						child.Set("title", stripDatePrefix(set.GetString("title")))
					case "uploader":
						// Union, not overwrite: overwriting would strip
						// co-uploaders off clips that other people contributed.
						// contents.uploader is single-valued (maxSelect 1), so
						// only fill it when empty.
						if child.GetString("uploader") == "" {
							if ups := set.GetStringSlice("uploader"); len(ups) > 0 {
								child.Set("uploader", ups[0])
							}
						}
					default:
						child.Set(field, set.Get(field))
					}
				}
				if err := txApp.Save(child); err != nil {
					return err
				}
				updated++
			}

			return nil
		})

		switch {
		case errors.Is(txErr, errSetNotFound):
			return e.NotFoundError("No such set.", nil)
		case errors.Is(txErr, errSetForbidden):
			return e.ForbiddenError("You can only edit sets you uploaded to.", nil)
		case errors.Is(txErr, errSetTooManyKids):
			return e.BadRequestError(
				fmt.Sprintf("This set has more than %d items.", maxPropagateChildren), nil)
		case txErr != nil:
			return e.InternalServerError("Could not propagate the set metadata.", txErr)
		}

		return e.JSON(200, map[string]any{
			"updated": updated,
			"fields":  fields,
		})
	}
}

// callerOwnsSet reports whether the user's uploader record is on the set.
//
// Mirrors contents_sets.deleteRule (uploader.user ?= @request.auth.id).
func callerOwnsSet(app core.App, set *core.Record, userID string) (bool, error) {
	uploaderIDs := set.GetStringSlice("uploader")
	if len(uploaderIDs) == 0 {
		return false, nil
	}
	var n int
	err := app.DB().
		NewQuery("SELECT COUNT(*) FROM {{uploaders}} WHERE [[id]] IN {:ids} AND [[user]] = {:user}").
		Bind(dbx.Params{"ids": uploaderIDs, "user": userID}).
		Row(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// stripDatePrefix removes the leading "YYMMDD " that set titles carry.
func stripDatePrefix(title string) string {
	prefix, rest, found := strings.Cut(title, " ")
	if !found || len(prefix) != 6 {
		return title
	}
	for _, r := range prefix {
		if r < '0' || r > '9' {
			return title
		}
	}
	return rest
}
