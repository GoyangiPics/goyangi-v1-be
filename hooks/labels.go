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

const (
	// maxLabelNameLen matches the `name` field's max in the schema.
	maxLabelNameLen = 50

	// maxLabelsPerContent is the real griefing bound. Without it one user can pin
	// fifty labels on a single item, and "cannot delete a label while it is in
	// use" means an admin then has to unpick them one at a time.
	maxLabelsPerContent = 30
)

// RegisterLabelHooks wires the user-created label layer.
//
// Labels are deliberately a separate namespace from the curated `tags`: anyone
// can create and apply a label, whereas tags are admin-only. The invariants are
// that a name is unique case-insensitively, and that a label cannot be deleted
// while any content still carries it.
//
// Three pieces below: slug normalisation (which is what makes uniqueness work),
// the in-use delete guard, and the mirror that keeps `contents.labels` in step
// with the join table.
func RegisterLabelHooks(app *pocketbase.PocketBase) {
	// Slug is computed server-side on create AND update, so the unique index on
	// `slug` enforces case-insensitive name uniqueness whichever path a record
	// arrived through — the apply endpoint, the admin UI, or a superuser API call.
	app.OnRecordCreate("labels").BindFunc(normalizeLabel)
	app.OnRecordUpdate("labels").BindFunc(normalizeLabel)

	app.OnRecordDelete("labels").BindFunc(func(e *core.RecordEvent) error {
		n, err := countLabelApplications(e.App, e.Record.Id)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("label %q is still applied to %d item(s)", e.Record.GetString("name"), n)
		}
		return e.Next()
	})

	// A model-hook error surfaces as a generic 400, so this adds the readable
	// message for API callers. The hook above is still the real guard — it also
	// covers the admin UI and every Go path.
	app.OnRecordDeleteRequest("labels").BindFunc(func(e *core.RecordRequestEvent) error {
		n, err := countLabelApplications(e.App, e.Record.Id)
		if err != nil {
			return err
		}
		if n > 0 {
			return e.BadRequestError(
				fmt.Sprintf("This label is still applied to %d item(s). Remove it from them first.", n),
				nil,
			)
		}
		return e.Next()
	})

	// Keep the denormalised contents.labels array in step with the join table.
	app.OnRecordAfterCreateSuccess("contents_labels").BindFunc(syncContentLabels)
	app.OnRecordAfterDeleteSuccess("contents_labels").BindFunc(syncContentLabels)

	// Cheap safety net for the denormalisation: rebuild any content whose array
	// length disagrees with its join count. 04:17 to stay clear of the hour.
	if err := app.Cron().Add("labels-reconcile", "17 4 * * *", func() {
		if err := reconcileContentLabels(app); err != nil {
			log.Printf("⚠️  labels: reconcile failed: %v", err)
		}
	}); err != nil {
		log.Printf("⚠️  labels: could not schedule reconcile: %v", err)
	}
}

// normalizeLabel trims the name and derives the slug.
//
// Slugify can return "" (a name with no alphanumerics, e.g. "!!!"), which would
// fail `slug`'s required check with an unhelpful message — rejected explicitly
// here instead.
func normalizeLabel(e *core.RecordEvent) error {
	name := strings.TrimSpace(e.Record.GetString("name"))
	slug := Slugify(name)
	if name == "" || slug == "" {
		return errors.New("label name must contain at least one letter or digit")
	}
	if len([]rune(name)) > maxLabelNameLen {
		return fmt.Errorf("label name must be %d characters or fewer", maxLabelNameLen)
	}
	e.Record.Set("name", name)
	e.Record.Set("slug", slug)
	return e.Next()
}

func countLabelApplications(app core.App, labelID string) (int, error) {
	var n int
	err := app.DB().
		NewQuery("SELECT COUNT(*) FROM {{contents_labels}} WHERE [[label]] = {:id}").
		Bind(dbx.Params{"id": labelID}).
		Row(&n)
	return n, err
}

// syncContentLabels rebuilds contents.labels from the join table.
//
// Rebuilt wholesale rather than incremented with +=/-= so it is idempotent and
// self-healing: a missed event or a partial failure corrects itself on the next
// change to the same content.
//
// The join table is the source of truth; contents.labels is a read cache that
// exists so listings can filter with `labels.slug ?= "x"` and expand one level,
// instead of chaining a two-level back-relation on every query. That is also why
// `labels` is in contents.updateRule's guard list — only this hook writes it.
func syncContentLabels(e *core.RecordEvent) error {
	contentID := e.Record.GetString("content")
	if contentID == "" {
		return e.Next()
	}

	rows, err := e.App.FindRecordsByFilter(
		"contents_labels", "content={:id}", "created", 0, 0,
		dbx.Params{"id": contentID},
	)
	if err != nil {
		return err
	}

	labelIDs := make([]string, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		id := row.GetString("label")
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		labelIDs = append(labelIDs, id)
	}

	content, err := e.App.FindRecordById("contents", contentID)
	if err != nil {
		// The content was deleted, which cascaded these rows away — nothing to sync.
		if errors.Is(err, sql.ErrNoRows) {
			return e.Next()
		}
		return err
	}

	content.Set("labels", labelIDs)
	// SaveNoValidate: this touches one relation array on an otherwise untouched
	// record, and re-running full validation on it would be wasted work.
	if err := e.App.SaveNoValidate(content); err != nil {
		return err
	}
	return e.Next()
}

// reconcileContentLabels repairs any drift between the join table and the
// denormalised array.
func reconcileContentLabels(app *pocketbase.PocketBase) error {
	type row struct {
		ID string `db:"id"`
	}
	var mismatched []row

	// json_array_length reads the relation column, which PocketBase stores as a
	// JSON array for multi-select relations.
	err := app.DB().NewQuery(`
		SELECT c.id AS id
		FROM ` + "`contents`" + ` c
		LEFT JOIN (
			SELECT content, COUNT(*) AS n FROM {{contents_labels}} GROUP BY content
		) cl ON cl.content = c.id
		WHERE COALESCE(json_array_length(COALESCE(NULLIF(c.labels,''),'[]')), 0) != COALESCE(cl.n, 0)
	`).All(&mismatched)
	if err != nil {
		return err
	}
	if len(mismatched) == 0 {
		return nil
	}

	log.Printf("🔧 labels: reconciling %d content record(s)", len(mismatched))
	for _, m := range mismatched {
		rows, err := app.FindRecordsByFilter(
			"contents_labels", "content={:id}", "created", 0, 0, dbx.Params{"id": m.ID},
		)
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(rows))
		for _, r := range rows {
			if id := r.GetString("label"); id != "" {
				ids = append(ids, id)
			}
		}
		content, err := app.FindRecordById("contents", m.ID)
		if err != nil {
			continue
		}
		content.Set("labels", ids)
		if err := app.SaveNoValidate(content); err != nil {
			log.Printf("⚠️  labels: reconcile could not save %s: %v", m.ID, err)
		}
	}
	return nil
}

// ─── Routes ─────────────────────────────────────────────────────────────────

type applyLabelBody struct {
	Content string `json:"content"`
	Name    string `json:"name"`
}

// RegisterLabelRoutes adds the label endpoints.
//
//	POST   /api/labels/apply        {content, name} -> {label, applied}
//	DELETE /api/admin/labels/{id}                   -> {deletedApplications}
//
// Removing a single application is deliberately NOT an endpoint: the frontend
// deletes its own `contents_labels` row under that collection's deleteRule,
// which is less code for the same result.
func RegisterLabelRoutes(app *pocketbase.PocketBase) {
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.POST("/api/labels/apply", applyLabel(app)).
			Bind(apis.RequireAuth("users"))

		e.Router.DELETE("/api/admin/labels/{id}", forceDeleteLabel(app)).
			Bind(apis.RequireAuth("users"), requireAdmin())

		return e.Next()
	})
}

// applyLabel is get-or-create plus apply, in one transaction.
//
// This is where a duplicate name becomes UX rather than an error: a name that
// already exists resolves to the existing label and is applied, never a 400. A
// free-tag namespace should be find-or-create — the user typed a name and gets
// that label.
//
// It is also the only way a non-superuser creates a label (labels.createRule is
// null), which is what guarantees the slug is always server-computed.
func applyLabel(app *pocketbase.PocketBase) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		var body applyLabelBody
		if err := e.BindBody(&body); err != nil {
			return e.BadRequestError("Invalid request body.", err)
		}

		name := strings.TrimSpace(body.Name)
		if name == "" || len([]rune(name)) > maxLabelNameLen {
			return e.BadRequestError(
				fmt.Sprintf("A label name is required and must be %d characters or fewer.", maxLabelNameLen),
				nil,
			)
		}
		slug := Slugify(name)
		if slug == "" {
			return e.BadRequestError("A label name must contain at least one letter or digit.", nil)
		}
		if body.Content == "" {
			return e.BadRequestError("A content id is required.", nil)
		}

		var (
			label   *core.Record
			applied bool
		)

		txErr := app.RunInTransaction(func(txApp core.App) error {
			if _, err := txApp.FindRecordById("contents", body.Content); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errLabelNoContent
				}
				return err
			}

			existingCount, err := countContentLabels(txApp, body.Content)
			if err != nil {
				return err
			}

			// Match on slug, not name: Slugify is what collapses "Cute", "cute"
			// and "CUTE!" onto one label, and the unique slug index is the race
			// backstop if two requests get here at once.
			found, err := txApp.FindFirstRecordByFilter(
				"labels", "slug={:slug}", dbx.Params{"slug": slug},
			)
			switch {
			case err == nil:
				label = found
			case errors.Is(err, sql.ErrNoRows):
				collection, cErr := txApp.FindCollectionByNameOrId("labels")
				if cErr != nil {
					return cErr
				}
				fresh := core.NewRecord(collection)
				fresh.Set("name", name)
				// normalizeLabel recomputes the slug on save.
				if sErr := txApp.Save(fresh); sErr != nil {
					return sErr
				}
				label = fresh
			default:
				return err
			}

			joinExists, err := txApp.FindFirstRecordByFilter(
				"contents_labels", "content={:c} && label={:l}",
				dbx.Params{"c": body.Content, "l": label.Id},
			)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if joinExists != nil {
				return nil // already applied — applied stays false
			}

			// Only counts against the cap when actually adding one.
			if existingCount >= maxLabelsPerContent {
				return errLabelCap
			}

			joinCollection, err := txApp.FindCollectionByNameOrId("contents_labels")
			if err != nil {
				return err
			}
			join := core.NewRecord(joinCollection)
			join.Set("content", body.Content)
			join.Set("label", label.Id)
			join.Set("user", e.Auth.Id)
			if err := txApp.Save(join); err != nil {
				return err
			}
			applied = true
			return nil
		})

		switch {
		case errors.Is(txErr, errLabelNoContent):
			return e.NotFoundError("No such content.", nil)
		case errors.Is(txErr, errLabelCap):
			return e.BadRequestError(
				fmt.Sprintf("This item already has the maximum of %d labels.", maxLabelsPerContent),
				nil,
			)
		case txErr != nil:
			return e.InternalServerError("Could not apply the label.", txErr)
		}

		return e.JSON(200, map[string]any{
			"label": map[string]string{
				"id":   label.Id,
				"name": label.GetString("name"),
				"slug": label.GetString("slug"),
			},
			"applied": applied,
		})
	}
}

// Sentinels, so the transaction can signal a specific HTTP status out to the
// handler without the closure capturing a response.
var (
	errLabelNoContent = errors.New("labels: no such content")
	errLabelNotFound  = errors.New("labels: no such label")
	errLabelCap       = errors.New("labels: per-content cap reached")
)

func countContentLabels(app core.App, contentID string) (int, error) {
	var n int
	err := app.DB().
		NewQuery("SELECT COUNT(*) FROM {{contents_labels}} WHERE [[content]] = {:id}").
		Bind(dbx.Params{"id": contentID}).
		Row(&n)
	return n, err
}

// forceDeleteLabel removes a label and every application of it.
//
// The escape hatch for the "cannot delete while in use" invariant. Without it,
// removing a slur applied to 40 items means 40 manual join-row deletions first,
// which makes the invariant a liability rather than a safeguard.
func forceDeleteLabel(app *pocketbase.PocketBase) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		id := e.Request.PathValue("id")
		if id == "" {
			return e.BadRequestError("A label id is required.", nil)
		}

		var deleted int
		txErr := app.RunInTransaction(func(txApp core.App) error {
			label, err := txApp.FindRecordById("labels", id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errLabelNotFound
				}
				return err
			}

			rows, err := txApp.FindRecordsByFilter(
				"contents_labels", "label={:id}", "created", 0, 0, dbx.Params{"id": id},
			)
			if err != nil {
				return err
			}

			// Applications first, so the in-use guard sees zero by the time the
			// label itself is deleted. Each delete fires syncContentLabels.
			for _, row := range rows {
				if err := txApp.Delete(row); err != nil {
					return err
				}
				deleted++
			}

			return txApp.Delete(label)
		})

		switch {
		case errors.Is(txErr, errLabelNotFound):
			return e.NotFoundError("No such label.", nil)
		case txErr != nil:
			return e.InternalServerError("Could not delete the label.", txErr)
		}

		return e.JSON(200, map[string]int{"deletedApplications": deleted})
	}
}
