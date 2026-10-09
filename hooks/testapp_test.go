package hooks

import (
	"os"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// newSchemaApp is a PocketBase test app carrying this repo's real schema
// (pb_schema.json, the hand-maintained export), for tests that exercise API
// rules and record hooks rather than pure functions. Starts from an empty data
// dir, so it's fast and needs no fixtures checked in.
func newSchemaApp(t *testing.T) *tests.TestApp {
	t.Helper()
	app, err := tests.NewTestAppWithConfig(core.BaseAppConfig{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("test app: %v", err)
	}
	t.Cleanup(app.Cleanup)

	schema, err := os.ReadFile("../pb_schema.json")
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if err := app.ImportCollectionsByMarshaledJSON(schema, false); err != nil {
		t.Fatalf("import schema: %v", err)
	}
	return app
}

func TestSchemaAppBoots(t *testing.T) {
	app := newSchemaApp(t)
	for _, name := range []string{"contents", "contents_sets", "uploaders", "users_likes"} {
		if _, err := app.FindCollectionByNameOrId(name); err != nil {
			t.Errorf("collection %s missing: %v", name, err)
		}
	}
}
