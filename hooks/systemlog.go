package hooks

import (
	"encoding/json"
	"log"
	"os"
	"slices"
	"strconv"
	"sync"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// System logging. Operational errors used to be reported into the Discord
// channel the media was posted in, which turned the scraping channels into a
// noticeboard. They now land in the "system_logs" collection instead, tagged
// with the subsystem that produced them so the two intake paths can be told
// apart in the dashboard.

const SystemLogsCollection = "system_logs"

// Log sources — which intake path produced the entry.
const (
	// LogSourceDiscordBot covers everything reaching us through Discord:
	// role-ping ingestion, @-mentions, replies and the context-menu command.
	LogSourceDiscordBot = "discord_bot"
	// LogSourceDirectUpload covers the web/API path: record-create validation,
	// the R2 transcode pipeline and the stateless /api/convert endpoints.
	LogSourceDirectUpload = "direct_upload"
	// LogSourceServer covers the process itself: boots, deploys, rollbacks.
	LogSourceServer = "server"
)

// Log levels. "error" means the operation failed; "warning" means it completed
// with something degraded (a missing preview, an unresolved name).
const (
	LogLevelError   = "error"
	LogLevelWarning = "warning"
	// LogLevelInfo is for events worth a durable record that aren't problems,
	// e.g. which commit a boot is running.
	LogLevelInfo = "info"
)

// Select values per field. ensureSystemLogsCollection adds any missing ones to
// an existing collection, so a value introduced later reaches old databases.
var (
	logSources = []string{LogSourceDiscordBot, LogSourceDirectUpload, LogSourceServer}
	logLevels  = []string{LogLevelError, LogLevelWarning, LogLevelInfo}
)

// logApp is set by RegisterSystemLogs. Nil until then, which makes every log
// call a no-op plus a stdout line — writes during early boot must not panic.
var (
	logApp   *pocketbase.PocketBase
	logAppMu sync.RWMutex
)

// RegisterSystemLogs wires the collection bootstrap. The collection is created
// on first boot if it doesn't exist, so a fresh deploy needs no manual step in
// the admin UI (this project has no migrations directory — pb_schema.json is
// an export, not a source of truth).
func RegisterSystemLogs(app *pocketbase.PocketBase) {
	logAppMu.Lock()
	logApp = app
	logAppMu.Unlock()

	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		// Collections can only be queried once the DB is up, so ensure runs
		// after the rest of the bootstrap chain.
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureSystemLogsCollection(app); err != nil {
			// Non-fatal: losing the log sink must not stop the app from serving.
			log.Printf("⚠️  system_logs: could not ensure collection: %v", err)
		}
		return nil
	})

	// Retention sweep. These entries exist to be reviewed and tuned against
	// (the text-detection log in particular is deliberately noisy), not to
	// accumulate forever — the collection is the only unbounded store of
	// operational data, and the privacy policy states a 90-day cap. 04:47 to
	// stay clear of the hour and of labels-reconcile.
	if err := app.Cron().Add("system-logs-retention", "47 4 * * *", func() {
		days := logRetentionDays()
		cutoff := types.NowDateTime().AddDate(0, 0, -days)
		// Raw SQL: rows carry no files and no hooks, and there may be
		// thousands per sweep — going through the ORM would fire a record
		// event per row for no benefit.
		res, err := app.DB().
			NewQuery("DELETE FROM {{" + SystemLogsCollection + "}} WHERE [[created]] < {:cutoff}").
			Bind(dbx.Params{"cutoff": cutoff.String()}).
			Execute()
		if err != nil {
			log.Printf("⚠️  system_logs: retention sweep failed: %v", err)
			return
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			log.Printf("🧹 system_logs: pruned %d entries older than %d days", n, days)
		}
	}); err != nil {
		log.Printf("⚠️  system_logs: could not schedule retention sweep: %v", err)
	}
}

// logRetentionDays reads GOYANGI_LOG_RETENTION_DAYS, defaulting to 90. Zero,
// negative and unparsable values fall back to the default rather than turning
// the sweep into a delete-everything.
func logRetentionDays() int {
	const fallback = 90
	raw := os.Getenv("GOYANGI_LOG_RETENTION_DAYS")
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func ensureSystemLogsCollection(app *pocketbase.PocketBase) error {
	if c, err := app.FindCollectionByNameOrId(SystemLogsCollection); err == nil {
		return ensureLogSelectValues(app, c)
	}

	c := core.NewBaseCollection(SystemLogsCollection)
	c.Fields.Add(&core.SelectField{
		Name:      "source",
		Required:  true,
		MaxSelect: 1,
		Values:    logSources,
	})
	c.Fields.Add(&core.SelectField{
		Name:      "level",
		Required:  true,
		MaxSelect: 1,
		Values:    logLevels,
	})
	c.Fields.Add(&core.TextField{Name: "message", Required: true, Max: 5000})
	// Free-form JSON blob: record id, filename, Discord message URL, etc.
	c.Fields.Add(&core.JSONField{Name: "context", MaxSize: 100_000})
	c.Fields.Add(&core.AutodateField{Name: "created", OnCreate: true})

	// All API rules left nil — superuser-only access. These entries can contain
	// internal paths and error strings and are not for public consumption.
	if err := app.Save(c); err != nil {
		return err
	}
	log.Printf("✅ system_logs: collection created")
	return nil
}

// ensureLogSelectValues appends select values added since the collection was
// created; saving a record with a value the field doesn't list fails.
func ensureLogSelectValues(app *pocketbase.PocketBase, c *core.Collection) error {
	changed := false
	for name, want := range map[string][]string{"source": logSources, "level": logLevels} {
		f, ok := c.Fields.GetByName(name).(*core.SelectField)
		if !ok {
			continue
		}
		for _, v := range want {
			if !slices.Contains(f.Values, v) {
				f.Values = append(f.Values, v)
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	return app.Save(c)
}

// writeSystemLog persists one entry. Failures are reported to stdout only —
// never back through this function, which would recurse.
func writeSystemLog(source, level, message string, ctx map[string]any) {
	logAppMu.RLock()
	app := logApp
	logAppMu.RUnlock()

	if app == nil {
		return
	}

	col, err := app.FindCollectionByNameOrId(SystemLogsCollection)
	if err != nil {
		log.Printf("⚠️  system_logs: collection unavailable: %v", err)
		return
	}

	rec := core.NewRecord(col)
	rec.Set("source", source)
	rec.Set("level", level)
	rec.Set("message", message)
	if len(ctx) > 0 {
		if b, mErr := json.Marshal(ctx); mErr == nil {
			rec.Set("context", string(b))
		}
	}

	if err := app.Save(rec); err != nil {
		log.Printf("⚠️  system_logs: could not write entry: %v", err)
	}
}

// LogServerInfo records a process-level event (boot, deploy, rollback).
func LogServerInfo(message string, ctx map[string]any) {
	log.Printf("ℹ️  %s %v", message, ctx)
	writeSystemLog(LogSourceServer, LogLevelInfo, message, ctx)
}

// LogServerWarning records a process-level event that needs attention, such as
// a deploy that was rolled back.
func LogServerWarning(message string, ctx map[string]any) {
	log.Printf("⚠️  %s %v", message, ctx)
	writeSystemLog(LogSourceServer, LogLevelWarning, message, ctx)
}

// LogUploadError records a failure in the direct-upload / transcode path.
func LogUploadError(message string, ctx map[string]any) {
	log.Printf("❌ %s %v", message, ctx)
	writeSystemLog(LogSourceDirectUpload, LogLevelError, message, ctx)
}

// LogUploadWarning records a degraded-but-completed direct upload.
func LogUploadWarning(message string, ctx map[string]any) {
	log.Printf("⚠️  %s %v", message, ctx)
	writeSystemLog(LogSourceDirectUpload, LogLevelWarning, message, ctx)
}

// LogBotError records a failure in the Discord ingestion path.
func LogBotError(message string, ctx map[string]any) {
	log.Printf("❌ %s %v", message, ctx)
	writeSystemLog(LogSourceDiscordBot, LogLevelError, message, ctx)
}

// LogBotWarning records a Discord ingestion that completed with problems
// (partial failures, unresolved idol/group names).
func LogBotWarning(message string, ctx map[string]any) {
	log.Printf("⚠️  %s %v", message, ctx)
	writeSystemLog(LogSourceDiscordBot, LogLevelWarning, message, ctx)
}
