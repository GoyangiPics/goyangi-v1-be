package hooks

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// One human, several uploader records.
//
// The two ingest doors mint uploaders independently: the bot calls
// lookupOrCreateByName with the poster's Discord username, the site creates one
// from the name the account chose. `uploaders.name` is not unique, so the same
// person posting imgur links in Discord and uploading on the site ends up as two
// records — and only the site one carries `user`, which is what every ownership
// check reads (contents.updateRule's `uploader.user = @request.auth.id`, and
// /me/uploads). So their Discord-ingested content is neither theirs to edit nor
// visible in their own uploads.
//
// Two halves here, and they are deliberately separate:
//
//   - `aliases` stops NEW duplicates. Same field and same purpose as on groups
//     and groups_idols, whose comment explains the pattern: what people type is
//     rarely the canonical name, and keeping the variants as data means a mod
//     fixes a miss by editing a record rather than shipping a deploy.
//   - The merge endpoint repairs the duplicates that already exist.

const (
	// maxMergeUploaderSources bounds one merge, matching maxMergeSources: the
	// whole thing is a single transaction holding SQLite's write lock, and the
	// encode pipeline writes on the same connection.
	maxMergeUploaderSources = 20

	// maxReassignContents bounds how many records one merge will repoint.
	// Exceeding it is an error rather than a silent partial merge.
	maxReassignContents = 5000
)

var (
	errUploaderNotFound     = errors.New("uploaders: no such uploader")
	errUploaderSelfMerge    = errors.New("uploaders: target cannot also be a source")
	errUploaderTooManyRefs  = errors.New("uploaders: too many records to reassign")
	errUploaderStillRefd    = errors.New("uploaders: source is still referenced")
	errUploaderNoSourceLeft = errors.New("uploaders: no usable source given")
)

// RegisterUploaderFields adds `aliases` and `blockIngest` to the uploaders
// collection.
//
// Programmatic for the reason ensureDimensionFields documents: this project has
// no migrations directory, so a deploy needing someone to add a field by hand
// first would silently keep creating duplicate uploaders until they did.
// Idempotent — a no-op on every boot after the first.
//
// pb_schema.json carries the same fields. It is a hand-maintained export, and
// leaving it behind would mean the next `pnpm typegen` in the frontend dropped
// the types this feature depends on.
func RegisterUploaderFields(app *pocketbase.PocketBase) {
	// After e.Next() and non-fatal, matching RegisterDimensionFields: collections
	// can only be queried once the DB is up, and a failure here must not stop the
	// app serving.
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureUploaderFields(app); err != nil {
			log.Printf("⚠️  uploaders: could not ensure uploader fields: %v", err)
		}
		return nil
	})
}

func ensureUploaderFields(app *pocketbase.PocketBase) error {
	collection, err := app.FindCollectionByNameOrId("uploaders")
	if err != nil {
		return fmt.Errorf("find uploaders: %w", err)
	}

	added := false
	if collection.Fields.GetByName("aliases") == nil {
		// Unconstrained text, mirroring the field on groups: a comma-separated
		// list with no length cap, not required. Nothing parses it but splitTrim.
		collection.Fields.Add(&core.TextField{Name: "aliases"})
		added = true
	}
	if collection.Fields.GetByName("blockIngest") == nil {
		// Flip this in the admin UI to stop the bot ingesting anything credited
		// to this uploader. Superuser-only: RegisterUploaderGuards restores it on
		// any other write — see the note on the check in bot/ingest.go.
		collection.Fields.Add(&core.BoolField{Name: "blockIngest"})
		added = true
	}
	if collection.Fields.GetByName("skipDiscordImport") == nil {
		// The uploader's own choice, unlike blockIngest: "don't turn my Discord
		// posts into posts automatically". For people who upload everything on
		// the site and post the same thing to Discord, which otherwise lands
		// twice. Only the passive path honours it; an explicit "Ingest this
		// message" or /reupload still goes through. False = import, as before.
		collection.Fields.Add(&core.BoolField{Name: "skipDiscordImport"})
		added = true
	}
	if !added {
		return nil
	}

	return app.Save(collection)
}

// RegisterUploaderGuards keeps the moderation fields of an uploader out of its
// owner's hands.
//
// uploaders.updateRule is `user = @request.auth.id`, which lets the owner write
// any field — including `blockIngest`, which is how an admin stops the bot
// ingesting for someone, and `aliases`, which decide whose Discord username is
// credited to whom: an owner adding someone else's name would be credited with
// that person's Discord posts. Rules can't scope a write to some fields, so the
// request hooks put those fields back. `user` too, so a profile can't be handed
// to another account. Superusers (the admin UI, the merge endpoint's own writes
// don't come through here at all) are unaffected.
//
// The owner keeps name and skipDiscordImport, which is all the site edits.
func RegisterUploaderGuards(app *pocketbase.PocketBase) {
	app.OnRecordCreateRequest("uploaders").BindFunc(func(e *core.RecordRequestEvent) error {
		if !e.HasSuperuserAuth() {
			e.Record.Set("aliases", "")
			e.Record.Set("blockIngest", false)
		}
		return e.Next()
	})

	app.OnRecordUpdateRequest("uploaders").BindFunc(func(e *core.RecordRequestEvent) error {
		if !e.HasSuperuserAuth() {
			original := e.Record.Original()
			for _, field := range []string{"aliases", "blockIngest", "user"} {
				e.Record.Set(field, original.Get(field))
			}
		}
		return e.Next()
	})
}

// ─── Merge ──────────────────────────────────────────────────────────────────

type mergeUploadersBody struct {
	Target  string   `json:"target"`
	Sources []string `json:"sources"`
	// KeepAliases folds each source's name (and its own aliases) into the
	// target's, so the bot resolves the old Discord username onto the surviving
	// record and cannot recreate the duplicate. Defaults to true; send false to
	// leave the target's aliases untouched.
	KeepAliases *bool `json:"keepAliases"`
}

// RegisterUploaderRoutes adds the uploader merge endpoint.
//
//	POST /api/admin/uploaders/merge  {target, sources[], keepAliases} (admin)
//
// Admin-only, and that is a security boundary rather than mere tidiness: an
// uploader's `user` is what grants edit and delete over every content record
// attached to it, so a self-service "that Discord name is me" would be a
// one-click takeover of somebody else's uploads. The claim has to be a decision
// someone makes, not one a caller asserts.
func RegisterUploaderRoutes(app *pocketbase.PocketBase) {
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.POST("/api/admin/uploaders/merge", mergeUploaders(app)).
			Bind(apis.RequireAuth("users"), requireAdmin())
		return e.Next()
	})
}

// mergeUploaders repoints every record from the source uploaders onto the
// target, then deletes the emptied sources.
//
// # Order, and the guard
//
// Same shape as mergeSets, for the same reason: references are moved FIRST and
// the sources are deleted LAST, each behind a count check taken inside the
// transaction. If a concurrent upload attaches to a source after its records
// were moved, that check turns silent data loss into a 409.
//
// Unlike sets there is no cascade to fear — nothing cascade-deletes from
// `uploaders`, so a stray reference would be left dangling rather than taking
// content with it. The guard stays because a dangling relation is still a broken
// record, and it is the difference between "nothing happened" and "one clip now
// has no uploader".
//
// Storage is untouched: record ids and R2 keys are built from date/group/idol
// (see generateContentId), so no object moves and no public URL changes.
func mergeUploaders(app *pocketbase.PocketBase) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		var body mergeUploadersBody
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Invalid request body.", err)
		}
		if body.Target == "" || len(body.Sources) == 0 {
			return e.BadRequestError("A target and at least one source are required.", nil)
		}
		if len(body.Sources) > maxMergeUploaderSources {
			return e.BadRequestError(
				fmt.Sprintf("At most %d source uploaders can be merged at once.", maxMergeUploaderSources), nil)
		}

		keepAliases := true
		if body.KeepAliases != nil {
			keepAliases = *body.KeepAliases
		}

		var (
			target       *core.Record
			movedContent int
			movedSets    int
			deleted      []string
			aliasesAdded []string
		)

		txErr := app.RunInTransaction(func(txApp core.App) error {
			var err error
			target, err = txApp.FindRecordById("uploaders", body.Target)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errUploaderNotFound
				}
				return err
			}

			// Load every source up front, so a bad id fails before any write.
			seen := map[string]struct{}{body.Target: {}}
			sources := make([]*core.Record, 0, len(body.Sources))
			for _, id := range body.Sources {
				if _, dup := seen[id]; dup {
					if id == body.Target {
						return errUploaderSelfMerge
					}
					continue // duplicate source id, harmless
				}
				seen[id] = struct{}{}
				src, err := txApp.FindRecordById("uploaders", id)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						return errUploaderNotFound
					}
					return err
				}
				sources = append(sources, src)
			}
			if len(sources) == 0 {
				return errUploaderNoSourceLeft
			}

			for _, src := range sources {
				// 1. contents.uploader is single-valued (maxSelect 1), so this is
				// a straight reassign.
				contents, err := txApp.FindRecordsByFilter(
					"contents", "uploader = {:id}", "created", maxReassignContents, 0,
					dbx.Params{"id": src.Id},
				)
				if err != nil {
					return err
				}
				if len(contents) >= maxReassignContents {
					return errUploaderTooManyRefs
				}
				for _, record := range contents {
					record.Set("uploader", target.Id)
					// SaveNoValidate: this touches one relation on records the
					// caller never sent, and full validation would let an
					// unrelated problem on one of them fail the whole merge —
					// the same reasoning the encode pipeline's final save uses.
					if err := txApp.SaveNoValidate(record); err != nil {
						return err
					}
					movedContent++
				}

				// 2. contents_sets.uploader is a MULTI relation (sets accumulate
				// co-uploaders as people add to them), so swap the id inside the
				// array and dedupe — the target may already be on the set.
				sets, err := txApp.FindRecordsByFilter(
					"contents_sets", "uploader ?= {:id}", "created", maxReassignContents, 0,
					dbx.Params{"id": src.Id},
				)
				if err != nil {
					return err
				}
				if len(sets) >= maxReassignContents {
					return errUploaderTooManyRefs
				}
				for _, set := range sets {
					next := make([]string, 0, len(set.GetStringSlice("uploader")))
					for _, id := range set.GetStringSlice("uploader") {
						if id == src.Id {
							continue
						}
						next = append(next, id)
					}
					set.Set("uploader", unionStrings(next, target.Id))
					if err := txApp.SaveNoValidate(set); err != nil {
						return err
					}
					movedSets++
				}
			}

			// 3. Absorb the names, so the bot resolves them onto the survivor.
			if keepAliases {
				srcNames := make([]uploaderNames, 0, len(sources))
				for _, src := range sources {
					srcNames = append(srcNames, uploaderNames{
						name:    src.GetString("name"),
						aliases: src.GetString("aliases"),
					})
				}
				joined, added := absorbAliases(
					target.GetString("name"), target.GetString("aliases"), srcNames)
				aliasesAdded = added
				if len(added) > 0 {
					target.Set("aliases", joined)
					if err := txApp.SaveNoValidate(target); err != nil {
						return err
					}
				}
			}

			// 4. Sources last, each guarded against a reference that arrived
			// while we were working.
			for _, src := range sources {
				// Single-valued, so a plain column comparison is exact.
				var remainingContents int
				if err := txApp.DB().NewQuery(
					"SELECT COUNT(*) FROM `contents` WHERE `uploader` = {:id}",
				).Bind(dbx.Params{"id": src.Id}).Row(&remainingContents); err != nil {
					return err
				}
				// Multi-valued: stored as a JSON array, so this goes through the
				// ORM's `?=` rather than a LIKE, which would also match an id that
				// merely contained this one as a substring.
				remainingSets, err := txApp.FindRecordsByFilter(
					"contents_sets", "uploader ?= {:id}", "created", 1, 0,
					dbx.Params{"id": src.Id},
				)
				if err != nil {
					return err
				}
				if remainingContents != 0 || len(remainingSets) != 0 {
					return fmt.Errorf("%w: %s (%d contents, %d sets)",
						errUploaderStillRefd, src.Id, remainingContents, len(remainingSets))
				}
				if err := txApp.Delete(src); err != nil {
					return err
				}
				deleted = append(deleted, src.Id)
			}

			return nil
		})

		switch {
		case errors.Is(txErr, errUploaderNotFound):
			return e.NotFoundError("One of the uploaders does not exist.", nil)
		case errors.Is(txErr, errUploaderSelfMerge):
			return e.BadRequestError("The target uploader cannot also be a source.", nil)
		case errors.Is(txErr, errUploaderNoSourceLeft):
			return e.BadRequestError("No usable source uploader was given.", nil)
		case errors.Is(txErr, errUploaderTooManyRefs):
			return e.BadRequestError(
				fmt.Sprintf("An uploader has more than %d records — merge it in the admin UI.", maxReassignContents), nil)
		case errors.Is(txErr, errUploaderStillRefd):
			// 409, not 500: nothing was lost, the guard did its job.
			return apis.NewApiError(409,
				"A source uploader gained records during the merge. Nothing was changed — try again.", nil)
		case txErr != nil:
			return e.InternalServerError("Could not merge the uploaders.", txErr)
		}

		log.Printf("🔀 uploaders: merged %v into %s (%d contents, %d sets)",
			deleted, target.Id, movedContent, movedSets)

		return e.JSON(200, map[string]any{
			"target":       target.Id,
			"name":         target.GetString("name"),
			"movedContent": movedContent,
			"movedSets":    movedSets,
			"deleted":      deleted,
			"aliasesAdded": aliasesAdded,
			"aliases":      target.GetString("aliases"),
		})
	}
}

// uploaderNames is one record's name and alias list, as the merge reads them.
type uploaderNames struct {
	name    string
	aliases string
}

// absorbAliases folds each source's name and aliases into the target's alias
// list, returning the whole list and, separately, only what was added.
//
// This is what makes a merge self-preventing: the bot resolves an uploader
// through recordNames, so once the old Discord username is an alias of the
// surviving record, the next ingest finds it instead of creating the duplicate
// again.
func absorbAliases(targetName, targetAliases string, sources []uploaderNames) (string, []string) {
	aliases := splitTrimStrings(targetAliases)
	var added []string

	for _, src := range sources {
		for _, candidate := range append([]string{src.name}, splitTrimStrings(src.aliases)...) {
			candidate = strings.TrimSpace(candidate)
			if candidate == "" {
				continue
			}
			// A comma would be read as two aliases by every splitTrim on the read
			// side. Reachable through a source NAME, which nothing splits — not
			// through its aliases, which were split on commas to get here.
			if strings.Contains(candidate, ",") {
				continue
			}
			// Aliasing a record to its own name is noise, and matching is
			// case-insensitive so it would never be consulted anyway.
			if strings.EqualFold(candidate, targetName) {
				continue
			}
			if containsFold(aliases, candidate) {
				continue
			}
			aliases = append(aliases, candidate)
			added = append(added, candidate)
		}
	}

	return strings.Join(aliases, ", "), added
}

// splitTrimStrings splits a comma-separated list, dropping empties.
//
// The bot has its own splitTrim; this is the hooks-side copy rather than a
// dependency, because hooks must not import bot (see OnAvifReady's note on the
// import cycle).
func splitTrimStrings(value string) []string {
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// containsFold reports whether the list already holds the value, ignoring case —
// alias matching is case-insensitive, so "Nabi" and "nabi" are one alias.
func containsFold(list []string, value string) bool {
	for _, item := range list {
		if strings.EqualFold(item, value) {
			return true
		}
	}
	return false
}
