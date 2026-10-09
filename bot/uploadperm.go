package bot

import (
	"log/slog"
	"strings"

	"github.com/pocketbase/pocketbase/core"
)

// Whether a Discord user is allowed to put content on the site.
//
// The manual ingestion paths — /reupload and "Ingest this message" — publish
// under the caller's name from any channel, so "can this person upload?" has to
// be a real answer rather than an assumption. The passive triggers are not
// gated this way: they credit the message AUTHOR, who never asked for anything,
// and they are already confined to channels somebody curated.
//
// There is no Discord id on `uploaders`, so the link is by name: the caller's
// Discord display name (or username) has to match an uploaders record's `name`
// or one of its `aliases`, and that record's `user` relation has to point at a
// users account with canUpload set.

// uploadDenial explains why a caller may not upload, or is empty if they may.
//
// A string rather than an error: every one of these is a message shown straight
// back to the person, and each names the specific thing to fix — "no" on its own
// would leave someone guessing which of three links is missing.
type uploadDenial string

// callerUploadDenial reports why the named Discord user may not upload. On
// success (an empty denial) it also hands back the matched record's canonical
// `name`, and the caller MUST credit that name rather than any of the ones it
// passed in.
//
// The two would drift otherwise: the gate matches on display name OR username,
// by name or alias, while the pipeline's lookupOrCreateByName CREATES an
// uploader for any name it doesn't know. A caller whose record matched via
// username but who was credited by display name would mint a stray, unlinked
// uploader on the first run — and be locked out on the second, when the gate
// finds the stray (display name checks first) and sees no account behind it.
//
// Fails CLOSED, unlike uploaderIngestFlags, which deliberately fails open. The
// two guard opposite things: that one is a quality filter where losing content
// to a transient error is the worse outcome, this one is a permission check
// where letting an unknown account publish is.
func callerUploadDenial(names ...string) (uploadDenial, string) {
	record, err := findUploaderByNames(names...)
	if err != nil {
		slog.Error("could not read uploaders for the upload permission check", "err", err)
		return "I couldn't check your upload permission just now — try again shortly.", ""
	}
	if record == nil {
		return "Your Discord name isn't linked to a goyangi uploader. Ask an admin to add it.", ""
	}

	userID := record.GetString("user")
	if userID == "" {
		return uploadDenial("The uploader **" + record.GetString("name") + "** isn't linked to a goyangi account. Ask an admin to link it."), ""
	}

	user, err := App.FindRecordById("users", userID)
	if err != nil {
		slog.Warn("uploader points at a missing user", "uploader", record.Id, "user", userID, "err", err)
		return uploadDenial("The goyangi account linked to **" + record.GetString("name") + "** no longer exists. Ask an admin to relink it."), ""
	}
	if !user.GetBool("canUpload") {
		return "Your goyangi account doesn't have upload permission. Request access on the site first.", ""
	}

	// A record can match via an alias while its `name` sits empty — schema
	// allows it. Falling back keeps the credit non-empty; lookupOrCreateByName
	// still resolves it to this record by that same alias.
	name := record.GetString("name")
	if name == "" && len(names) > 0 {
		name = names[0]
	}
	return "", name
}

// findUploaderByNames returns the first uploaders record matching any of the
// given names, by `name` or by an alias. Nil (with no error) when none match.
//
// Deliberately does NOT create one, unlike lookupOrCreateByName: creating an
// uploader here would manufacture the very record the permission check is
// asking about, and it would pass on the next attempt with no account behind it.
func findUploaderByNames(names ...string) (*core.Record, error) {
	records, err := App.FindRecordsByFilter("uploaders", "", "-created", 0, 0)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		needle := strings.ToLower(strings.TrimSpace(name))
		if needle == "" {
			continue
		}
		for _, r := range records {
			for _, candidate := range recordNames(r) {
				if candidate == needle {
					return r, nil
				}
			}
		}
	}
	return nil, nil
}
