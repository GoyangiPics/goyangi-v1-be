package hooks

import (
	"fmt"
	"log"
	"net/url"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// Provenance values for the `origin` field on contents and contents_sets.
//
// `imgur` is a site upload whose file was fetched from an imgur link by the
// upload page's import, rather than picked from disk. It used to be stamped
// `direct`, which was unfair to the people who actually upload their own files
// — and it is a different kind of thing: recovered from elsewhere, with the
// imgur link kept as `mirror`. Discord-ingested imgur links stay `discord`;
// the bot is the provenance there, imgur just the host.
//
// `twitter` is the same kind of import from an X post: the upload page fetched
// the post's media through the FxTwitter API instead of the uploader picking
// files from disk. The post's link is the record's `source`, and that link is
// the condition for the claim (see siteClaimsTwitter).
//
// `script` is `discord` with a handle on it: what scripts/backfilldiscord writes,
// so a backfill can be inspected, relabelled to `discord` once trusted (the
// script's -relabel mode), or removed wholesale if not. Nothing else writes it,
// and nothing treats it differently from `discord`.
const (
	OriginDirect  = "direct"
	OriginDiscord = "discord"
	OriginImgur   = "imgur"
	OriginTwitter = "twitter"
	OriginScript  = "script"
)

// claimableOrigins are the values a superuser may state over the API
// (provenance.go). A site user can claim neither.
var claimableOrigins = map[string]bool{OriginDiscord: true, OriginScript: true}

// isImgurMirror reports whether a `mirror` value points at imgur — the only
// thing the upload page ever sets `mirror` to (see uploadPlan.ts), and the tell
// that a site upload was an import rather than a file from disk.
func isImgurMirror(mirror string) bool {
	u, err := url.Parse(strings.TrimSpace(mirror))
	if err != nil || u.Host == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	return host == "imgur.com" || strings.HasSuffix(host, ".imgur.com")
}

// isXStatusURL reports whether a link is an X (Twitter) post:
// https://x.com/<user>/status/<id>, or the same on twitter.com.
func isXStatusURL(link string) bool {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return false
	}
	switch strings.TrimPrefix(strings.TrimPrefix(strings.ToLower(u.Hostname()), "www."), "mobile.") {
	case "x.com", "twitter.com":
	default:
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 3 || parts[0] == "" || parts[1] != "status" || parts[2] == "" {
		return false
	}
	for _, r := range parts[2] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// siteClaimsTwitter reports whether a site upload says it was imported from an
// X post and backs that with the post as its `source`.
//
// The one claim a site user may make on a post, and a cosmetic one: it decides
// a chip and a filter value, nothing about access. Without the link there is
// nothing to back it, and it is ignored like any other claim.
func siteClaimsTwitter(record *core.Record) bool {
	return record.GetString("origin") == OriginTwitter && isXStatusURL(record.GetString("source"))
}

// apiOrigin is the provenance of a record arriving over the API: an import if
// it carries an imgur mirror, a direct upload otherwise. Never Discord — that
// claim is the bot's alone.
func apiOrigin(record *core.Record) string {
	if isImgurMirror(record.GetString("mirror")) {
		return OriginImgur
	}
	return OriginDirect
}

// ensureOriginValues adds the later-introduced values ("imgur", "script") to the
// `origin` select on both collections. Programmatic for the same reason
// contents.width/height are (see dimensions.go): there is no migrations
// directory, and a value the field does not list is rejected on save.
func ensureOriginValues(app *pocketbase.PocketBase) error {
	for _, name := range []string{"contents", "contents_sets"} {
		collection, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return fmt.Errorf("find %s: %w", name, err)
		}
		field, ok := collection.Fields.GetByName("origin").(*core.SelectField)
		if !ok {
			return fmt.Errorf("%s.origin is not a select field", name)
		}
		var added []string
		for _, want := range []string{OriginImgur, OriginScript, OriginTwitter} {
			has := false
			for _, v := range field.Values {
				if v == want {
					has = true
				}
			}
			if !has {
				field.Values = append(field.Values, want)
				added = append(added, want)
			}
		}
		if len(added) == 0 {
			continue
		}
		if err := app.Save(collection); err != nil {
			return fmt.Errorf("save %s: %w", name, err)
		}
		log.Printf("🔧 schema: added %q to %s.origin", added, name)
	}
	return nil
}

// RegisterOriginHooks makes `origin` server-authoritative.
//
// Before this field existed, the frontend inferred provenance from `mirror`,
// which is only set for imgur links — so Discord attachments, catbox and
// pixeldrain all rendered as direct uploads. The backend already had the right
// signal (a non-empty `discord` means the bot ingested it, see bot/autopost.go);
// this promotes that to an explicit field that `contents_sets` can carry too,
// since a set has no `discord` of its own.
//
// The frontend never writes it. API callers cannot claim "discord", and once set
// it is immutable through the API.
func RegisterOriginHooks(app *pocketbase.PocketBase) {
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureOriginValues(app); err != nil {
			log.Printf("⚠️  schema: origin values: %v", err)
		}
		return nil
	})
	bindOriginHooks(app)
}

// bindOriginHooks is the request and model half, split out so tests can bind
// it on a test app without the bootstrap step.
func bindOriginHooks(app core.App) {

	// OnRecordCreateRequest fires around the API handler, i.e. BEFORE the
	// model-level OnRecordCreate below. Anything arriving over the API is a site
	// upload by definition — direct, or an imgur import, told apart by `mirror`,
	// which the upload page sets only for imports. The caller's own claim is
	// ignored, with two exceptions: an X import, backed by the post as `source`
	// (siteClaimsTwitter), and a SUPERUSER's "discord" or "script" for a record
	// that carries a Discord jump link (provenance.go explains who and why).
	app.OnRecordCreateRequest("contents").BindFunc(func(e *core.RecordRequestEvent) error {
		switch claimed := claimedOrigin(e); {
		case claimed != "" && e.Record.GetString("discord") != "":
			e.Record.Set("origin", claimed)
		case siteClaimsTwitter(e.Record):
			e.Record.Set("origin", OriginTwitter)
		default:
			e.Record.Set("origin", apiOrigin(e.Record))
		}
		return e.Next()
	})

	// A set has no mirror to read, so the upload page says which kind of batch
	// it is. Only "imgur" and "twitter" are accepted from the caller; anything
	// else is a direct upload, and "discord" in particular stays the bot's to claim — and the
	// superuser's (provenance.go), since a set has no link of its own to check.
	app.OnRecordCreateRequest("contents_sets").BindFunc(func(e *core.RecordRequestEvent) error {
		switch claimed := claimedOrigin(e); {
		case claimed != "":
			e.Record.Set("origin", claimed)
		case e.Record.GetString("origin") != OriginImgur && e.Record.GetString("origin") != OriginTwitter:
			e.Record.Set("origin", OriginDirect)
		}
		return e.Next()
	})

	// Immutable via the API. Restoring the previous value (rather than rejecting
	// the request) keeps unrelated edits working — the per-clip edit menu sends a
	// whole form, and provenance is not the user's to change. The superuser's
	// "discord" claim is the same exception as on create, so a record written
	// before that claim was possible can be corrected.
	app.OnRecordUpdateRequest("contents", "contents_sets").BindFunc(func(e *core.RecordRequestEvent) error {
		if claimed := claimedOrigin(e); claimed != "" && (e.Collection.Name == "contents_sets" || e.Record.GetString("discord") != "") {
			e.Record.Set("origin", claimed)
		} else {
			e.Record.Set("origin", e.Record.Original().GetString("origin"))
		}
		return e.Next()
	})

	// Model-level default, so records created in Go (the bot) get a value without
	// each call site remembering to set one.
	app.OnRecordCreate("contents").BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetString("origin") == "" {
			if e.Record.GetString("discord") != "" {
				e.Record.Set("origin", OriginDiscord)
			} else {
				e.Record.Set("origin", apiOrigin(e.Record))
			}
		}
		return e.Next()
	})

	// Sets have no `discord` field to derive from, so the bot sets this
	// explicitly in createSetRecord; this is the fallback for anything else.
	app.OnRecordCreate("contents_sets").BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetString("origin") == "" {
			e.Record.Set("origin", OriginDirect)
		}
		return e.Next()
	})
}
