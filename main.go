package main

import (
	"log"
	"os"

	"goyangi-v1-be/bot"
	"goyangi-v1-be/hooks"

	"github.com/joho/godotenv"
	// Loads .env during package initialization, before hooks and bot evaluate
	// their package-level vars (MAX_PROCESS_JOBS, the bot's channel lists, …).
	// Go initializes packages in import-path order as far as dependencies
	// allow, and github.com/joho/... sorts ahead of goyangi-v1-be/..., so these
	// see .env values. The explicit Load in main() alone ran too late for them.
	_ "github.com/joho/godotenv/autoload"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// commit is the git commit this binary was built from, stamped by
// scripts/updater's build (-ldflags "-X main.commit=<sha>"). "dev" otherwise.
var commit = "dev"

func main() {
	// Railway captures stdout and stamps its own timestamps.
	log.SetOutput(os.Stdout)
	log.SetFlags(0)

	// Already loaded by the autoload import above; repeated only for the log
	// line. godotenv never overrides a variable that is already set.
	if err := godotenv.Load(); err != nil {
		log.Println("ℹ️  No .env file found, using system environment")
	}

	app := pocketbase.New()

	// First, so a crash anywhere after this is captured: the runtime writes its
	// trace to pb_data/crash.log and the next boot files it in system_logs.
	hooks.RegisterCrashReport(app)

	// Operational error sink ("system_logs" collection). Registered first so
	// the other subsystems can log during their own setup.
	hooks.RegisterSystemLogs(app)
	// Boot-time check that ffmpeg/ffprobe and the encoders uploads need are
	// present, and which H.264 encoder (VAAPI/NVENC/CPU) was picked.
	hooks.RegisterPreflight(app)
	// Logs the running commit to system_logs, and — only when started by
	// scripts/updater — lets it ask for a clean exit once nothing is in flight.
	hooks.RegisterDeployInfo(app, commit)
	hooks.RegisterDrain(app)
	// R2 storage hooks (upload → transcode + custom path, delete → cleanup).
	hooks.RegisterR2Hooks(app)
	// Failed or interrupted encodes: recorded on the post, retryable, re-queued at boot.
	hooks.RegisterEncodeRecovery(app)
	// Server-authoritative `origin` (direct vs discord) on contents/sets.
	hooks.RegisterOriginHooks(app)
	// contents/contents_sets.date falls back to the upload time when unset.
	hooks.RegisterContentDateDefaults(app)
	// A superuser may state `created` and origin "discord" over the API — the
	// backfill of pre-bot posts needs both (hooks/provenance.go).
	hooks.RegisterProvenanceOverrides(app)
	// One-off data migrations, run once at boot and idempotent thereafter.
	hooks.RegisterBackfills(app)
	// contents.width/height, added programmatically because there is no
	// migrations directory — without them every upload would store nothing.
	hooks.RegisterDimensionFields(app)
	// uploaders.aliases, same reason: without it the bot keeps minting a second
	// uploader for anyone whose Discord name differs from their site name.
	hooks.RegisterUploaderFields(app)
	// groups_idols.birthday, same reason — and the admin UI can't add it on
	// production, where saving a collection there crashes the server.
	hooks.RegisterIdolFields(app)
	// Owners may rename their uploader, not unblock it or claim aliases.
	hooks.RegisterUploaderGuards(app)
	// Who may change which posts and sets: access rules, derived set uploaders,
	// empty-set cleanup, likes guard, admin audit.
	hooks.RegisterOwnership(app)
	// `uploaders_stats` view + contents(uploader, filetype) index: the
	// /uploaders directory in one request instead of one count per uploader.
	hooks.RegisterUploaderStats(app)
	// Stateless conversion endpoints (POST /api/convert/avif, etc.).
	hooks.RegisterConvertRoutes(app)
	// POST /api/views/{collection}/{id} — unauthenticated view counter.
	hooks.RegisterViewRoutes(app)
	// GET /api/queue — encode queue depth, for the upload page's "N ahead of
	// yours" hint. Must come after RegisterR2Hooks, which logs the queue size.
	hooks.RegisterQueueRoutes(app)
	// User-created label layer: slug normalisation, in-use delete guard,
	// contents.labels mirror, reconcile cron.
	hooks.RegisterLabelHooks(app)
	// POST /api/labels/apply, DELETE /api/admin/labels/{id}.
	hooks.RegisterLabelRoutes(app)
	// POST /api/admin/sets/merge, POST /api/sets/{id}/propagate.
	hooks.RegisterSetRoutes(app)
	// POST /api/admin/uploaders/merge — folds duplicate uploader profiles
	// (Discord-ingested vs site-created) into one.
	hooks.RegisterUploaderRoutes(app)
	// Blocks adding content to a collection the caller doesn't own. A hook
	// rather than an API rule — rules can't see relation modifiers, see the file.
	hooks.RegisterCollectionGuards(app)
	// GDPR erasure on account deletion: anonymize the uploader credit, drop the
	// public links rows and solely-owned collections the cascade leaves behind.
	hooks.RegisterUserDeleteHooks(app)
	// One name per person: email sign-up needs one, and the uploader name is
	// copied onto the account (hooks/usernames.go).
	hooks.RegisterUsernameHooks(app)
	// Wired here to avoid a hooks → bot import cycle. The two announcers post
	// through our own bot, so without it they aren't wired at all — otherwise
	// each notice would sit in a batch for nothing, holding up restarts.
	if bot.Enabled() {
		hooks.OnAvifReady = bot.QueueAvifNotification
		hooks.OnContentPublished = bot.QueueUploadPost
	}
	hooks.BotBusy = bot.Busy

	// Two doors into the same Discord ingestion: our own bot (unless
	// DISCORD_BOT_ENABLED=false) and the ingest API, through which another bot
	// forwards the messages it reads (only with INGEST_API_TOKEN set).
	bot.Init(app)
	bot.RegisterIngestAPI(app)

	// Start the Discord bot once PocketBase is bootstrapped and serving.
	if bot.Enabled() {
		app.OnServe().BindFunc(func(e *core.ServeEvent) error {
			go func() {
				log.Println("🤖 Starting Discord bot...")
				if err := bot.Start(); err != nil {
					log.Println("❌ Failed to start Discord bot:", err)
				}
			}()
			return e.Next()
		})
	} else {
		log.Println("ℹ️  Discord bot disabled (DISCORD_BOT_ENABLED=false)")
	}

	if err := app.Start(); err != nil {
		log.Fatal(err)
	}
}
