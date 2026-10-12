package hooks

import (
	"fmt"
	"log"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// birthdayFieldId matches pb_schema.json, so a later schema import sees the
// field this code created as the same field rather than a replacement.
const birthdayFieldId = "date_birthday_groups_idols"

// RegisterIdolFields ensures groups_idols.birthday exists, once, at boot.
//
// Same shape as RegisterDimensionFields: after e.Next(), non-fatal. Without the
// field the site simply shows no birthday announcements.
func RegisterIdolFields(app *pocketbase.PocketBase) {
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureIdolBirthdayField(app); err != nil {
			log.Printf("⚠️  idols: could not ensure groups_idols.birthday: %v", err)
		}
		return nil
	})
}

// ensureIdolBirthdayField adds the optional `birthday` date to groups_idols.
//
// In code rather than an admin-UI edit for the usual reason (no migrations
// directory) and one more: on production every collection save from the admin
// UI crashed the server — a stack overflow while decoding the request body
// into the collection (core.Collection.UnmarshalJSON recursing through the
// json/v2 engine of that host's Go toolchain). app.Save on a collection built
// in Go never decodes JSON, so it takes a different path.
func ensureIdolBirthdayField(app *pocketbase.PocketBase) error {
	collection, err := app.FindCollectionByNameOrId("groups_idols")
	if err != nil {
		return fmt.Errorf("find groups_idols: %w", err)
	}
	if collection.Fields.GetByName("birthday") != nil {
		return nil
	}
	// Optional, no bounds: most idols are filled by scripts/backfillbirthdays,
	// and a new idol may be added before anyone knows the date.
	collection.Fields.Add(&core.DateField{Id: birthdayFieldId, Name: "birthday"})
	if err := app.Save(collection); err != nil {
		return err
	}
	LogServerInfo("added groups_idols.birthday", nil)
	return nil
}
