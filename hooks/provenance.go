package hooks

import (
	"log"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/types"
)

// A superuser may state a record's real provenance over the API.
//
// Two facts about a content record are server-controlled for good reason:
// `created` is an autodate the API overwrites with "now" whatever the caller
// sends, and `origin` cannot be claimed as "discord" by an API caller
// (origin.go). Both rules exist so that site users can't forge provenance. The
// operator is not a site user: scripts/backfilldiscord archives posts made
// months before the bot existed, and those records are wrong in both respects
// unless somebody with the keys is allowed to say when a thing was posted and
// where it came from.
//
// So: for a SUPERUSER request only, on the two content collections, a
// `created` sent in the CreatedHeader is kept, and an origin of "discord" or
// "script" in the body is honoured when the record carries a Discord jump link (a set has none, and
// is taken at its word — it is written by the same superuser in the same run).
// Everything else is unchanged, and a regular user's request never reaches any
// of this.
//
// Hooks rather than a route: the write is still an ordinary record create or
// update, with every rule and every other hook applied — this only decides two
// fields. See origin.go for the discord half; this file owns `created`.

// CreatedHeader carries the wanted `created`, in any layout types.ParseDateTime
// accepts. A header and not a body field on purpose: PocketBase's request
// parsing blanks autodate fields in the body before any hook sees them (a
// `created` in the JSON arrives as an empty string in RequestInfo().Body), which
// is also why sending it in the body has never worked, superuser or not.
const CreatedHeader = "X-Goyangi-Created"

// RegisterProvenanceOverrides installs the `created` half.
func RegisterProvenanceOverrides(app *pocketbase.PocketBase) {
	app.OnRecordCreateRequest("contents", "contents_sets").BindFunc(keepRequestedCreated)
	app.OnRecordUpdateRequest("contents", "contents_sets").BindFunc(keepRequestedCreated)
}

// keepRequestedCreated writes the `created` a superuser asked for, after the save.
//
// After, not before: the autodate field's own interceptor runs inside the save
// and replaces whatever the record holds with "now", so a value set beforehand
// is gone by the time the row exists. Raw SQL on the committed row is the one
// point the field no longer has a say, and it deliberately bumps nothing else —
// no `updated`, no hooks, no realtime event — the same reasoning as
// backfillOrigin.
//
// The response body still carries the autodate's value; the caller re-reads if
// it cares (the backfill does, once, as a check that this hook is deployed).
func keepRequestedCreated(e *core.RecordRequestEvent) error {
	if !e.HasSuperuserAuth() {
		return e.Next()
	}
	raw := e.Request.Header.Get(CreatedHeader)
	if raw == "" {
		return e.Next()
	}
	want, err := types.ParseDateTime(raw)
	if err != nil || want.IsZero() {
		return e.BadRequestError(CreatedHeader+" is not a datetime.", err)
	}

	// An undated record falls back to its upload time (content_date.go), and
	// that hook runs inside the save, where `created` is still "now". The
	// requested one is the upload time meant, so it stands in for the date here.
	if e.Record.GetDateTime("date").IsZero() {
		e.Record.Set("date", want)
	}

	if err := e.Next(); err != nil {
		return err
	}

	// Collection names come from the hook registration above, not the request.
	_, err = e.App.DB().NewQuery("UPDATE `" + e.Collection.Name + "` SET created = {:created} WHERE id = {:id}").
		Bind(dbx.Params{"created": want.String(), "id": e.Record.Id}).
		Execute()
	if err != nil {
		// The record is saved and correct in every other respect; a wrong
		// `created` is recoverable by the same request again. Log, don't fail.
		log.Printf("⚠️  provenance: could not set created on %s/%s: %v", e.Collection.Name, e.Record.Id, err)
	}
	return nil
}

// claimedOrigin is the origin a superuser request asks for, if it is one a
// superuser may claim (claimableOrigins), else "". Used by the origin hooks;
// always "" for everyone else.
func claimedOrigin(e *core.RecordRequestEvent) string {
	if !e.HasSuperuserAuth() {
		return ""
	}
	info, err := e.RequestInfo()
	if err != nil {
		return ""
	}
	v, _ := info.Body["origin"].(string)
	if !claimableOrigins[v] {
		return ""
	}
	return v
}
