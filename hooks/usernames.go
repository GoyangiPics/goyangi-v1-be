package hooks

import (
	"log"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// One name per person: `users.name`, kept equal to the uploader name.
//
// A person had two names. `users.name` is what the site shows for an account —
// the owner chip on a collection, a tools link's author, a report in the admin
// queue — and only Discord sign-up ever filled it (the OAuth2 mapping copies
// the Discord display name). Email sign-up asked for no name at all, so those
// accounts showed up blank everywhere. `uploaders.name` is the one people
// actually choose and edit (profile, the first-upload dialog), and nothing
// tied the two together, so even a Discord account's chip could disagree with
// the name on its posts.
//
// Three pieces:
//
//   - Email sign-up must carry a name. A request hook rather than a createRule:
//     OAuth2 sign-up creates its record through a plain Save, not the create
//     request, so this never touches Discord accounts — they arrive named.
//   - The uploader name is copied onto the account whenever it is set, so the
//     uploader name wins wherever the two had drifted.
//   - A boot backfill names the accounts that predate this from their uploader.
//     Accounts with neither are prompted by the site on their next visit.

func RegisterUsernameHooks(app *pocketbase.PocketBase) {
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := backfillUsernames(app); err != nil {
			log.Printf("⚠️  backfill: usernames failed: %v", err)
		}
		return nil
	})
	bindUsernameHooks(app)
}

// bindUsernameHooks is the request and model half, split out so tests can bind
// it on a test app without the bootstrap backfill.
func bindUsernameHooks(app core.App) {
	app.OnRecordCreateRequest("users").BindFunc(func(e *core.RecordRequestEvent) error {
		if e.HasSuperuserAuth() {
			return e.Next()
		}
		name := strings.TrimSpace(e.Record.GetString("name"))
		if name == "" {
			return apis.NewBadRequestError("Choose a username.", nil)
		}
		taken, err := usernameTaken(e.App, name, "")
		if err != nil {
			return err
		}
		if taken {
			return apis.NewBadRequestError("That username is taken.", nil)
		}
		e.Record.Set("name", name)
		return e.Next()
	})

	// Changing it is fine; clearing it would undo the sign-up requirement.
	app.OnRecordUpdateRequest("users").BindFunc(func(e *core.RecordRequestEvent) error {
		if e.HasSuperuserAuth() {
			return e.Next()
		}
		name := strings.TrimSpace(e.Record.GetString("name"))
		if name == e.Record.Original().GetString("name") {
			return e.Next()
		}
		if name == "" {
			return apis.NewBadRequestError("Choose a username.", nil)
		}
		taken, err := usernameTaken(e.App, name, e.Record.Id)
		if err != nil {
			return err
		}
		if taken {
			return apis.NewBadRequestError("That username is taken.", nil)
		}
		e.Record.Set("name", name)
		return e.Next()
	})

	// After success, so a rejected uploader write (the unique name index) leaves
	// the account alone. A failure here is logged, not returned: the uploader is
	// already saved, and the account catches up on the next rename.
	syncFromUploader := func(e *core.RecordEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		uid := e.Record.GetString("user")
		name := strings.TrimSpace(e.Record.GetString("name"))
		if uid == "" || name == "" {
			return nil
		}
		user, err := e.App.FindRecordById("users", uid)
		if err != nil {
			log.Printf("⚠️  usernames: uploader %s: user %s: %v", e.Record.Id, uid, err)
			return nil
		}
		if user.GetString("name") == name {
			return nil
		}
		user.Set("name", name)
		if err := e.App.Save(user); err != nil {
			log.Printf("⚠️  usernames: could not sync %s onto user %s: %v", name, uid, err)
		}
		return nil
	}
	app.OnRecordAfterCreateSuccess("uploaders").BindFunc(syncFromUploader)
	app.OnRecordAfterUpdateSuccess("uploaders").BindFunc(syncFromUploader)
}

// usernameTaken reports whether another person already goes by `name`, case
// folded the way uploaders' unique index folds it. Uploaders are checked too,
// so a name picked at sign-up can later become that person's uploader name;
// an uploader belonging to `self` doesn't count against them.
//
// Existing duplicates among Discord accounts are left alone — two people can
// share a Discord display name, and it is only email sign-ups and renames that
// pass through here.
func usernameTaken(app core.App, name, self string) (bool, error) {
	params := dbx.Params{"name": name, "self": self}
	var n int
	err := app.DB().NewQuery(
		"SELECT (SELECT COUNT(*) FROM {{users}} WHERE name = {:name} COLLATE NOCASE AND id != {:self})" +
			" + (SELECT COUNT(*) FROM {{uploaders}} WHERE name = {:name} COLLATE NOCASE" +
			" AND ({:self} = '' OR COALESCE([[user]], '') != {:self}))",
	).Bind(params).Row(&n)
	return n > 0, err
}

// backfillUsernames names every account that has none from its uploader.
//
// Raw SQL for the reasons backfillOrigin gives: no hooks, no `updated` bump.
// When merges left a person several uploaders, the oldest named one wins.
func backfillUsernames(app core.App) error {
	res, err := app.DB().NewQuery(
		"UPDATE {{users}} SET name = (" +
			"SELECT u.name FROM {{uploaders}} u WHERE u.[[user]] = {{users}}.id AND u.name != ''" +
			" ORDER BY u.created LIMIT 1)" +
			" WHERE COALESCE(name, '') = '' AND EXISTS (" +
			"SELECT 1 FROM {{uploaders}} u WHERE u.[[user]] = {{users}}.id AND u.name != '')",
	).Execute()
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("🏷️  backfill: named %d account(s) from their uploader", n)
	}
	return nil
}
