package hooks

import (
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// RegisterContentDateDefaults keeps `date` — the content's own "actual" date —
// set on every content and set record, falling back to the upload time.
//
// The frontend sorts and filters by `date` when the viewer picks "Actual", and
// PocketBase sorts empty values first with no way to fall back to another field
// in a sort expression. So an undated record has to carry a date or "oldest by
// actual date" opens on every undated item ever uploaded. The upload time is
// the best stand-in there is, and the one r2.go's recordDateString already
// falls back to when naming files.
//
// No flag marks a filled-in date. Wherever "has its own date" matters, the
// question is whether `date` falls on a different day from `created`: when it
// doesn't, a filled-in date and an explicitly chosen one say the same thing.
//
// Model-level, so the bot and scripts are covered as well as the API. Records
// from before these hooks are dated by scripts/backfilldate, run by hand.
func RegisterContentDateDefaults(app *pocketbase.PocketBase) {
	// `created` is stamped at save time, after these hooks run, so a new record
	// takes "now" — the same instant to within the save.
	app.OnRecordCreate("contents", "contents_sets").BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetDateTime("date").IsZero() {
			e.Record.Set("date", types.NowDateTime())
		}
		return e.Next()
	})

	// The edit forms send an empty date when it is cleared; clearing means
	// "no date of its own", which is the upload time again.
	app.OnRecordUpdate("contents", "contents_sets").BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetDateTime("date").IsZero() {
			e.Record.Set("date", e.Record.GetDateTime("created"))
		}
		return e.Next()
	})
}
