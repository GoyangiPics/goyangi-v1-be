package hooks

import (
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// viewTables maps the URL segment to a table name.
//
// Hardcoded so the path parameter is NEVER interpolated into SQL. Identifiers
// cannot be bound as parameters, so the only safe source for one is a fixed map.
var viewTables = map[string]string{
	"contents":      "contents",
	"contents_sets": "contents_sets",
}

// RegisterViewRoutes adds the view-counter endpoint.
//
//	POST /api/views/{collection}/{id}  ->  200 {"views": <new value>}
//
// A custom route rather than a client-side update, because it is the only thing
// that works: contents.updateRule requires auth AND blocks `views` for
// non-owners, while /single/[id] and /set/[id] are public. A read-modify-write
// from the client would also lose concurrent increments.
//
// Deliberately unauthenticated — most traffic is anonymous and this is a
// popularity signal, not an audit trail. No dedupe, by decision: a refresh
// counts again, and abuse is bounded by the rate limiter rather than by identity.
// Add a rule in the admin UI under Settings -> Rate limits with the label
// "POST /api/views/" (a trailing slash makes it a prefix rule); custom routes
// inherit the global rate-limit middleware automatically.
func RegisterViewRoutes(app *pocketbase.PocketBase) {
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		// Go 1.22 path syntax — {param}, not :param.
		e.Router.POST("/api/views/{collection}/{id}", incrementViews(app))
		return e.Next()
	})
}

func incrementViews(app *pocketbase.PocketBase) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		table, ok := viewTables[e.Request.PathValue("collection")]
		if !ok {
			return e.NotFoundError("Unknown view target.", nil)
		}

		id := e.Request.PathValue("id")
		// 200 is the `id` field's max length in the schema; anything longer
		// cannot match a record and is not worth a query.
		if id == "" || len(id) > 200 {
			return e.BadRequestError("Missing or invalid id.", nil)
		}

		// Atomic in one statement, so concurrent views can't clobber each other.
		// COALESCE because the column is nullable on rows that predate it.
		//
		// Two deliberate consequences of going around the ORM: no realtime event
		// is emitted, and `updated` is not bumped. The second matters — every
		// listing sorts by -created or -likes:length, and bumping `updated` on
		// every page view would be a write amplification for nothing. Both will
		// look like bugs to the next reader, hence this note.
		res, err := app.DB().NewQuery(
			"UPDATE `" + table + "` SET views = COALESCE(views, 0) + 1 WHERE id = {:id}",
		).Bind(dbx.Params{"id": id}).Execute()
		if err != nil {
			return e.InternalServerError("Could not record the view.", err)
		}

		// RowsAffected doubles as the existence check — no extra SELECT needed.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return e.NotFoundError("No such record.", nil)
		}

		var views int
		if err := app.DB().NewQuery(
			"SELECT COALESCE(views, 0) FROM `" + table + "` WHERE id = {:id}",
		).Bind(dbx.Params{"id": id}).Row(&views); err != nil {
			// The increment already landed; failing here would make the client
			// think it didn't and retry.
			return e.JSON(200, map[string]any{})
		}

		return e.JSON(200, map[string]int{"views": views})
	}
}
