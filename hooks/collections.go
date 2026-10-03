package hooks

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// RegisterCollectionGuards stops content being written into a collection the
// caller doesn't own.
//
// The hole this closes: `contents.createRule` is only `canUpload = true`, and
// `collections` is deliberately absent from `updateRule`'s guard list — that
// omission is what lets anyone add somebody ELSE'S content to their OWN
// collection, which is the whole point of the feature. But nothing checked the
// other direction, so a crafted request could add content to a STRANGER'S
// collection, and nothing in the UI would ever show them where it came from.
//
// # Why this is a hook and not an API rule
//
// The obvious fix is a rule clause like
//
//	@request.body.collections.user.id = @request.auth.id
//
// and it does not work. The app adds and removes membership with PocketBase's
// relation modifiers (`collections+` / `collections-`, see QuickCollectionModal),
// and rules cannot see those keys:
//
//   - RequestEvent.initRequestInfo binds the RAW body, with no modifier
//     normalisation — so the body key is literally "collections+" and
//     `@request.body.collections:isset` is FALSE.
//   - The resolver's allowed-identifier pattern is `\@request\.body\.[\w\.\:]*\w+`,
//     and `+` is not a word character, so `@request.body.collections+` cannot even
//     be expressed.
//
// A rule would therefore pass every request the app actually sends, while
// looking like it guarded them. A hook runs after modifiers are resolved and
// sees the record's real final state, whichever way it was written.
//
// Only ADDITIONS are checked. Removing content from a collection stays open,
// including someone else's: the legitimate case — "get my content out of your
// collection" — is indistinguishable from vandalism at this layer, and taking it
// away would break the former to slow down the latter.
func RegisterCollectionGuards(app *pocketbase.PocketBase) {
	app.OnRecordCreateRequest("contents").BindFunc(func(e *core.RecordRequestEvent) error {
		if err := guardCollectionOwnership(e); err != nil {
			return err
		}
		return e.Next()
	})

	app.OnRecordUpdateRequest("contents").BindFunc(func(e *core.RecordRequestEvent) error {
		if err := guardCollectionOwnership(e); err != nil {
			return err
		}
		return e.Next()
	})
}

// guardCollectionOwnership rejects the request when it adds the record to a
// collection the caller does not own.
func guardCollectionOwnership(e *core.RecordRequestEvent) error {
	// Superusers and admins moderate; the whole point of those roles is reaching
	// records that aren't theirs.
	if e.HasSuperuserAuth() {
		return nil
	}
	if e.Auth == nil {
		// No auth at all can't pass the API rules either; let those answer.
		return nil
	}
	if e.Auth.GetBool("isAdmin") {
		return nil
	}

	// Original() is the pre-save DB state, and is empty on create — so on create
	// every id counts as added, which is what we want.
	added := addedStrings(e.Record.Original().GetStringSlice("collections"), e.Record.GetStringSlice("collections"))
	if len(added) == 0 {
		return nil
	}

	userID := e.Auth.Id
	for _, collectionID := range added {
		owned, err := userOwnsCollection(e.App, collectionID, userID)
		if err != nil {
			return e.InternalServerError("Could not verify the collection.", err)
		}
		if !owned {
			return e.ForbiddenError("You can only add content to your own collections.", nil)
		}
	}
	return nil
}

// addedStrings returns the values present in next but not in prev.
func addedStrings(prev, next []string) []string {
	before := make(map[string]struct{}, len(prev))
	for _, v := range prev {
		before[v] = struct{}{}
	}
	var added []string
	for _, v := range next {
		if v == "" {
			continue
		}
		if _, seen := before[v]; !seen {
			added = append(added, v)
			before[v] = struct{}{} // also dedupes within next
		}
	}
	return added
}

// userOwnsCollection reports whether the user is one of a collection's owners.
//
// `contents_collections.user` is a MULTI relation (maxSelect 999), so this is
// "is the caller among the owners", not "is the caller the owner" — which is
// also the reason an API rule couldn't express it even setting the modifier
// problem aside: `=` would demand every owner be the caller, and `?=` would pass
// as long as any ONE referenced collection matched, letting a foreign id ride
// along in the same request.
func userOwnsCollection(app core.App, collectionID, userID string) (bool, error) {
	record, err := app.FindRecordById("contents_collections", collectionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// A nonexistent collection is not the caller's; the relation would
			// fail validation later anyway, but this gives the honest reason.
			return false, nil
		}
		return false, fmt.Errorf("collection %s: %w", collectionID, err)
	}
	for _, owner := range record.GetStringSlice("user") {
		if strings.TrimSpace(owner) == userID {
			return true, nil
		}
	}
	return false, nil
}
