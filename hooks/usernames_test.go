package hooks

import (
	"net/http"
	"testing"
)

const usersURL = "/api/collections/users/records"

func TestEmailSignUpNeedsAFreeName(t *testing.T) {
	f := newFixture(t)
	bindUsernameHooks(f.app)

	signUp := func(extra string) string {
		return `{"email":"new@example.test","password":"password123","passwordConfirm":"password123"` + extra + `}`
	}
	f.do(t, "no name", http.MethodPost, usersURL, signUp(""), "", 400, "Chooseausername")
	f.do(t, "blank name", http.MethodPost, usersURL, signUp(`,"name":"   "`), "", 400, "Chooseausername")
	// The fixture's uploaders are named u1/u2/admin; the check folds case.
	f.do(t, "an uploader's name", http.MethodPost, usersURL, signUp(`,"name":"U1"`), "", 400, "taken")
	f.do(t, "a free name", http.MethodPost, usersURL, signUp(`,"name":"  nabi "`), "", 200, `"name":"nabi"`)
	f.do(t, "another account's name", http.MethodPost, usersURL,
		`{"email":"two@example.test","password":"password123","passwordConfirm":"password123","name":"NABI"}`,
		"", 400, "taken")
}

func TestUsernameCannotBeCleared(t *testing.T) {
	f := newFixture(t)
	bindUsernameHooks(f.app)
	u := usersURL + "/" + f.users["u2"].Id

	// u2's own uploader is named u2, so taking that name is no clash.
	f.do(t, "own uploader's name", http.MethodPatch, u, `{"name":"u2"}`, "u2", 200)
	f.do(t, "someone else's", http.MethodPatch, u, `{"name":"u1"}`, "u2", 400, "taken")
	f.do(t, "clearing it", http.MethodPatch, u, `{"name":""}`, "u2", 400, "Chooseausername")
	f.do(t, "an unrelated edit", http.MethodPatch, u, `{}`, "u2", 200)
}

func TestUploaderNameIsCopiedOntoTheAccount(t *testing.T) {
	f := newFixture(t)
	bindUsernameHooks(f.app)

	f.do(t, "rename uploader", http.MethodPatch,
		"/api/collections/uploaders/records/"+f.up["u1"], `{"name":"yujin_fan"}`, "u1", 200)
	if got := f.record(t, "users", f.users["u1"].Id).GetString("name"); got != "yujin_fan" {
		t.Errorf("users.name = %q, want the uploader name", got)
	}
}

func TestBackfillNamesAccountsFromTheirUploader(t *testing.T) {
	f := newFixture(t)
	// The fixture's accounts have no name; each has an uploader named after it.
	named := f.record(t, "users", f.users["u2"].Id)
	named.Set("name", "kept")
	mustSave(t, f.app, named)

	if err := backfillUsernames(f.app); err != nil {
		t.Fatal(err)
	}
	if got := f.record(t, "users", f.users["u1"].Id).GetString("name"); got != "u1" {
		t.Errorf("u1 name = %q, want u1", got)
	}
	if got := f.record(t, "users", f.users["u2"].Id).GetString("name"); got != "kept" {
		t.Errorf("an existing name was overwritten: %q", got)
	}
}
