package hooks

import (
	"log"
	"os"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// RegisterBackfills runs one-off data migrations at boot.
//
// Follows the ensureSystemLogsCollection pattern: after e.Next(), so the DB is
// up, and non-fatal, because a failed backfill must not stop the app serving.
// Each step is idempotent and guarded by a cheap count, so this is a no-op on
// every boot after the first.
func RegisterBackfills(app *pocketbase.PocketBase) {
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := backfillOrigin(app); err != nil {
			log.Printf("⚠️  backfill: origin failed: %v", err)
		}
		if err := backfillImgurOrigin(app); err != nil {
			log.Printf("⚠️  backfill: imgur origin failed: %v", err)
		}
		// Opt-in for now: run it by booting once with the variable set. Safe to
		// leave set afterwards — it is count-guarded like the others.
		if os.Getenv(contentDateBackfillEnv) == "1" {
			if err := backfillContentDate(app); err != nil {
				log.Printf("⚠️  backfill: content date failed: %v", err)
			}
		}
		return nil
	})
}

// backfillOrigin stamps `origin` on rows that predate the field.
//
// Raw SQL rather than an ORM loop, which is the first raw SQL in this package
// and worth justifying: an ORM pass over every content row at boot would be slow
// and would fire every record hook, including the R2 cleanup ones. It also
// deliberately does not bump `updated` — a backfill must not reorder anything.
//
// `set` is backticked because it is a SQLite keyword.
func backfillOrigin(app *pocketbase.PocketBase) error {
	var pending int
	err := app.DB().NewQuery(
		"SELECT (SELECT COUNT(*) FROM {{contents}} WHERE COALESCE(origin,'') = '')" +
			" + (SELECT COUNT(*) FROM {{contents_sets}} WHERE COALESCE(origin,'') = '')",
	).Row(&pending)
	if err != nil {
		return err
	}
	if pending == 0 {
		return nil
	}

	log.Printf("🔧 backfill: stamping origin on %d row(s)", pending)

	return app.RunInTransaction(func(txApp core.App) error {
		// contents: the Discord jump link is the tell — it is written only on
		// ingest, and site uploads announced back to Discord never get one.
		if _, err := txApp.DB().NewQuery(
			"UPDATE `contents` SET origin =" +
				" CASE WHEN COALESCE(discord,'') != '' THEN 'discord' ELSE 'direct' END" +
				" WHERE COALESCE(origin,'') = ''",
		).Execute(); err != nil {
			return err
		}

		// Sets take the origin of their OLDEST child, i.e. how the set started.
		//
		// A mixed set has no correct answer. This is the deterministic reading
		// that matches what the home-feed filter asks ("where did this set come
		// from"), and it avoids the alternative's failure mode: "any Discord
		// child wins" would relabel a site set that someone later reply-ingested
		// a single item into. If an only-direct filter ever shows something
		// surprising, this is why.
		_, err := txApp.DB().NewQuery(
			"UPDATE `contents_sets` SET origin = COALESCE((" +
				"  SELECT CASE WHEN COALESCE(c.discord,'') != '' THEN 'discord' ELSE 'direct' END" +
				"  FROM `contents` c WHERE c.`set` = `contents_sets`.id" +
				"  ORDER BY c.created ASC LIMIT 1" +
				"), 'direct')" +
				" WHERE COALESCE(origin,'') = ''",
		).Execute()
		return err
	})
}

// backfillImgurOrigin relabels the site uploads that were imgur imports.
//
// Until `imgur` existed as a value, every upload-page import was stamped
// `direct`. The import is the only site path that sets `mirror` (see
// uploadPlan.ts), and it sets it to the imgur link, so a direct row with an
// imgur mirror is an import. Discord rows are untouched: the bot sets `mirror`
// for imgur links too, but its provenance is Discord.
//
// Same shape as backfillOrigin: raw SQL, no `updated` bump, count-guarded so it
// is a no-op after the first boot. Runs after ensureOriginValues has added the
// value, or the rows would fail validation on their next save.
func backfillImgurOrigin(app *pocketbase.PocketBase) error {
	const imgurDirect = "origin = 'direct' AND (mirror LIKE '%://imgur.com/%' OR mirror LIKE '%.imgur.com/%')"

	var pending int
	if err := app.DB().NewQuery(
		"SELECT COUNT(*) FROM {{contents}} WHERE " + imgurDirect,
	).Row(&pending); err != nil {
		return err
	}
	if pending == 0 {
		return nil
	}

	log.Printf("🔧 backfill: relabelling %d imgur import(s) from direct", pending)

	return app.RunInTransaction(func(txApp core.App) error {
		if _, err := txApp.DB().NewQuery(
			"UPDATE `contents` SET origin = 'imgur' WHERE " + imgurDirect,
		).Execute(); err != nil {
			return err
		}
		// Sets follow their oldest child, as in backfillOrigin.
		_, err := txApp.DB().NewQuery(
			"UPDATE `contents_sets` SET origin = 'imgur' WHERE origin = 'direct' AND (" +
				"  SELECT c.origin FROM `contents` c WHERE c.`set` = `contents_sets`.id" +
				"  ORDER BY c.created ASC LIMIT 1" +
				") = 'imgur'",
		).Execute()
		return err
	})
}
