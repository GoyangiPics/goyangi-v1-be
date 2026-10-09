package hooks

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// Failed and interrupted encodes: recorded, visible, retryable.
//
// The encode pipeline (moveFileToCustomR2Path) runs once, from the create hook,
// in an in-memory goroutine. When it failed it logged to system_logs and
// returned, leaving the post with no renditions forever and nothing on the
// record to say so; a restart mid-queue lost every waiting job the same way.
// The source upload survives a failure (it's deleted only after a successful
// encode), so all of these are recoverable — they just needed a way back in.
//
// Every run now goes through startEncode, which counts the attempt
// (`encodeAttempts`) and, when the run ends without a preview, records why
// (`encodeError`). Owners and admins can retry from the site
// (POST /api/contents/{id}/reprocess), and boot re-queues what a restart
// interrupted. Both fields are written only here — the access rules lock them.

// maxEncodeAttempts caps automatic retries and owner retries. A file that has
// failed this often fails for a reason a retry won't fix; admins can still
// force one.
const maxEncodeAttempts = 3

// bootRequeueMinAge leaves recent posts alone at boot: during a deploy the old
// process may still be finishing them.
const bootRequeueMinAge = 10 * time.Minute

// encoder runs the pipeline for one post. Production wires it to
// moveFileToCustomR2Path; tests swap in a fake.
type encoder struct {
	app core.App
	run func(recordID string)
	// done, when set, is called once a queued run has fully finished,
	// bookkeeping included. Tests wait on it.
	done func(recordID string)
}

var (
	postEncoder     *encoder
	encodesInFlight sync.Map // record id → struct{}
)

// RegisterEncodeRecovery adds the status fields, the retry route and the boot
// re-queue. The encoder itself is wired by RegisterR2Hooks, so uploads keep
// encoding even if this isn't registered.
func RegisterEncodeRecovery(app *pocketbase.PocketBase) {
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureEncodeFields(app); err != nil {
			log.Printf("⚠️  encode: could not ensure status fields: %v", err)
		}
		return nil
	})

	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		registerReprocessRoute(e)
		if err := e.Next(); err != nil {
			return err
		}
		go requeueInterrupted(app)
		return nil
	})
}

func registerReprocessRoute(e *core.ServeEvent) {
	e.Router.POST("/api/contents/{id}/reprocess", reprocessContent).Bind(apis.RequireAuth("users"))
}

func ensureEncodeFields(app core.App) error {
	collection, err := app.FindCollectionByNameOrId("contents")
	if err != nil {
		return err
	}
	added := false
	if collection.Fields.GetByName("encodeError") == nil {
		collection.Fields.Add(&core.TextField{Name: "encodeError"})
		added = true
	}
	if collection.Fields.GetByName("encodeAttempts") == nil {
		collection.Fields.Add(&core.NumberField{Name: "encodeAttempts", OnlyInt: true})
		added = true
	}
	if !added {
		return nil
	}
	return app.Save(collection)
}

// startEncode queues one run of the pipeline for a post, unless one is already
// queued or running. Reports whether it queued.
func startEncode(recordID, filetype string) bool {
	if postEncoder == nil {
		return false
	}
	if _, busy := encodesInFlight.LoadOrStore(recordID, struct{}{}); busy {
		return false
	}
	// Counted now, synchronously: the job is in the queue from this moment, and
	// GET /api/queue must say so. See enqueueJob.
	release := enqueueJob(filetype)
	enc := postEncoder
	go func() {
		defer func() {
			encodesInFlight.Delete(recordID)
			release()
			if enc.done != nil {
				enc.done(recordID)
			}
		}()
		runEncode(enc, recordID)
	}()
	return true
}

func runEncode(enc *encoder, recordID string) {
	app := enc.app
	// Raw SQL for the bookkeeping: no hooks, no `updated` bump, no realtime
	// event — these are notes about the record, not edits to it.
	if _, err := app.DB().NewQuery(
		"UPDATE {{contents}} SET [[encodeAttempts]] = COALESCE([[encodeAttempts]], 0) + 1, " +
			"[[encodeError]] = '' WHERE [[id]] = {:id}",
	).Bind(dbx.Params{"id": recordID}).Execute(); err != nil {
		log.Printf("⚠️  encode: could not count attempt for %s: %v", recordID, err)
	}

	enc.run(recordID)

	record, err := app.FindRecordById("contents", recordID)
	if err != nil || record.GetString("preview") != "" {
		return // deleted meanwhile, or done
	}
	reason := lastUploadError(app, recordID)
	if reason == "" {
		reason = "Processing failed."
	}
	if _, err := app.DB().NewQuery(
		"UPDATE {{contents}} SET [[encodeError]] = {:reason} WHERE [[id]] = {:id}",
	).Bind(dbx.Params{"id": recordID, "reason": truncate(reason, 500)}).Execute(); err != nil {
		log.Printf("⚠️  encode: could not record failure for %s: %v", recordID, err)
	}
}

// lastUploadError is the newest system_logs message about this record — the
// pipeline logs every failure there with the record id in its context — so the
// stored reason is the real one rather than a generic "failed".
func lastUploadError(app core.App, recordID string) string {
	var message string
	err := app.DB().NewQuery(
		"SELECT [[message]] FROM {{" + SystemLogsCollection + "}} " +
			"WHERE [[level]] = 'error' AND [[context]] LIKE {:pattern} ORDER BY [[created]] DESC LIMIT 1",
	).Bind(dbx.Params{"pattern": `%"record":"` + recordID + `"%`}).Row(&message)
	if err != nil {
		return ""
	}
	return message
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// reprocessContent: POST /api/contents/{id}/reprocess.
//
// A route, not a rule-gated field write: what it does is run the pipeline, and
// no record write can start that. Owner or admin; only for a post that has its
// source but no renditions.
func reprocessContent(e *core.RequestEvent) error {
	if postEncoder == nil {
		return e.InternalServerError("Processing isn't available.", nil)
	}
	record, err := e.App.FindRecordById("contents", e.Request.PathValue("id"))
	if err != nil {
		return e.NotFoundError("No such post.", nil)
	}

	isAdmin := e.Auth.GetBool("isAdmin")
	if !isAdmin && !slices.Contains(callerUploaderIDs(e.App, e.Auth.Id), record.GetString("uploader")) {
		return e.ForbiddenError("You can only retry your own posts.", nil)
	}
	switch {
	case record.GetString("preview") != "":
		return e.BadRequestError("This post is already processed.", nil)
	case record.GetString("file") == "":
		return apis.NewApiError(409, "The original file is gone. Upload it again.", nil)
	case !isAdmin && record.GetInt("encodeAttempts") >= maxEncodeAttempts:
		return apis.NewApiError(409,
			fmt.Sprintf("This post failed %d times. Ask an admin, or upload it again.", maxEncodeAttempts), nil)
	}
	if !startEncode(record.Id, record.GetString("filetype")) {
		return apis.NewApiError(409, "This post is already processing.", nil)
	}
	return e.JSON(202, map[string]any{"queued": true})
}

// requeueInterrupted picks up posts a restart left without renditions: their
// source is still there, nothing ever finished them, and they have retries left.
func requeueInterrupted(app core.App) {
	cutoff := types.NowDateTime().Add(-bootRequeueMinAge)
	records, err := app.FindRecordsByFilter(
		"contents",
		"file != '' && preview = '' && encodeAttempts < {:max} && created < {:cutoff}",
		"created", 0, 0,
		dbx.Params{"max": maxEncodeAttempts, "cutoff": cutoff.String()},
	)
	if err != nil {
		log.Printf("⚠️  encode: could not list interrupted posts: %v", err)
		return
	}
	queued := 0
	for _, r := range records {
		if startEncode(r.Id, r.GetString("filetype")) {
			queued++
		}
	}
	if queued > 0 {
		LogServerInfo(fmt.Sprintf("Re-queued %d post(s) left unprocessed", queued), map[string]any{
			"count": queued,
			"ids":   strings.Join(firstIDs(records, 20), ","),
		})
	}
}

func firstIDs(records []*core.Record, n int) []string {
	ids := make([]string, 0, min(n, len(records)))
	for i, r := range records {
		if i == n {
			break
		}
		ids = append(ids, r.Id)
	}
	return ids
}
