package hooks

import (
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/security"
)

// Account deletion is self-service (users.deleteRule), but PocketBase's
// cascade only covers the relations marked cascadeDelete (likes, stars,
// filters, reports). Three relations deliberately aren't, and each needs its
// own erasure treatment or the "delete my account" button doesn't actually
// erase the person:
//
//   - uploaders.user — the uploader record survives with `user` unset, which
//     keeps the person's public display name (often their Discord username,
//     plus every alias) attached to all their content forever. The content
//     itself stays by design; the NAME is the personal data, so it is replaced
//     with an anonymous placeholder and the aliases cleared.
//   - users_links.user — publicly listable rows of user-supplied links; they
//     identify the person, so they are deleted outright.
//   - contents_collections.user — a collection solely owned by the deleted
//     account would survive ownerless: if public it stays public, and its
//     update/delete rules (`user.id ?= @request.auth.id`) become unsatisfiable
//     for everyone. Solely-owned collections are deleted; co-owned ones just
//     lose the departing owner via the cascade's auto-unset.
//
// Ordering matters twice over. The lookups run BEFORE e.Next(): the delete's
// cascade auto-unsets every non-cascade `user` reference, after which these
// rows can no longer be found by owner. The mutations run AFTER e.Next(), and
// on FRESH copies: everything is inside the delete's transaction (an error
// here rolls the whole deletion back rather than half-erasing someone), and
// the pre-delete copies still carry the old `user` id, which by then dangles —
// saving one back would fail relation validation.

func RegisterUserDeleteHooks(app *pocketbase.PocketBase) {
	app.OnRecordDelete("users").BindFunc(func(e *core.RecordEvent) error {
		uid := e.Record.Id
		params := dbx.Params{"uid": uid}

		uploaders, err := e.App.FindRecordsByFilter("uploaders", "user = {:uid}", "", 0, 0, params)
		if err != nil {
			return err
		}
		// Merges can leave one person with several uploader records, so all of
		// them are collected, not just the first.
		uploaderIDs := make([]string, 0, len(uploaders))
		for _, u := range uploaders {
			uploaderIDs = append(uploaderIDs, u.Id)
		}

		links, err := e.App.FindRecordsByFilter("users_links", "user = {:uid}", "", 0, 0, params)
		if err != nil {
			return err
		}

		collections, err := e.App.FindRecordsByFilter(
			"contents_collections", "user.id ?= {:uid}", "", 0, 0, params,
		)
		if err != nil {
			return err
		}
		soleOwned := make([]*core.Record, 0, len(collections))
		for _, c := range collections {
			if owners := c.GetStringSlice("user"); len(owners) == 1 && owners[0] == uid {
				soleOwned = append(soleOwned, c)
			}
		}

		if err := e.Next(); err != nil {
			return err
		}

		for _, id := range uploaderIDs {
			u, err := e.App.FindRecordById("uploaders", id)
			if err != nil {
				return err
			}
			u.Set("name", "deleted-"+security.RandomStringWithAlphabet(8, "abcdefghijklmnopqrstuvwxyz0123456789"))
			u.Set("aliases", "")
			if err := e.App.Save(u); err != nil {
				return err
			}
		}

		for _, l := range links {
			if err := e.App.Delete(l); err != nil {
				return err
			}
		}
		for _, c := range soleOwned {
			if err := e.App.Delete(c); err != nil {
				return err
			}
		}

		return nil
	})
}
