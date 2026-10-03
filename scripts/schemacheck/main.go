// Command schemacheck dry-runs a pb_schema.json import against a PocketBase
// data directory and reports what the import would do.
//
// It exists because pb_schema.json is hand-maintained (there is no migrations
// directory) and an import that fails validation — a bad rule expression, a view
// query referencing a missing table, an index on a column that does not exist —
// is otherwise only discovered by pasting it into the admin UI of a live server.
//
// Point it at a COPY of pb_data, never the real one:
//
//	go run ./scripts/schemacheck -data /tmp/pb_data_copy -schema pb_schema.json
//
// The import runs in a transaction that is always rolled back unless -commit is
// passed, so the copy is left untouched either way.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

var errRollback = errors.New("schemacheck: intentional rollback")

func main() {
	dataDir := flag.String("data", "", "PocketBase data directory (use a copy!)")
	schemaPath := flag.String("schema", "pb_schema.json", "path to pb_schema.json")
	commit := flag.Bool("commit", false, "actually apply the import instead of rolling back")
	flag.Parse()

	if *dataDir == "" {
		log.Fatal("-data is required (point it at a COPY of pb_data)")
	}

	raw, err := os.ReadFile(*schemaPath)
	if err != nil {
		log.Fatalf("read schema: %v", err)
	}

	// Sanity-check the JSON shape before handing it to PocketBase, so a trivial
	// syntax problem produces a clear message rather than a validation dump.
	var parsed []map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		log.Fatalf("schema is not a JSON array of collections: %v", err)
	}
	fmt.Printf("schema: %d collections in %s\n", len(parsed), *schemaPath)

	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: *dataDir})
	if err := app.Bootstrap(); err != nil {
		log.Fatalf("bootstrap: %v", err)
	}
	defer app.ResetBootstrapState() //nolint:errcheck

	before := snapshot(app)

	runErr := app.RunInTransaction(func(txApp core.App) error {
		// deleteMissing=false: additive import, matching what the admin UI does
		// with its destructive checkbox left off.
		if err := txApp.ImportCollectionsByMarshaledJSON(raw, false); err != nil {
			return err
		}
		after := snapshot(txApp)
		report(before, after)
		if *commit {
			return nil
		}
		return errRollback
	})

	switch {
	case runErr == nil:
		fmt.Println("\nIMPORT OK (committed)")
	case errors.Is(runErr, errRollback):
		fmt.Println("\nIMPORT OK (rolled back — pass -commit to apply)")
	default:
		fmt.Printf("\nIMPORT FAILED: %v\n", runErr)
		os.Exit(1)
	}
}

type colInfo struct {
	kind    string
	fields  map[string]string
	indexes map[string]struct{}
	rules   map[string]string
}

func snapshot(app core.App) map[string]colInfo {
	out := map[string]colInfo{}
	cols := []*core.Collection{}
	if err := app.CollectionQuery().All(&cols); err != nil {
		return out
	}
	for _, c := range cols {
		info := colInfo{
			kind:    c.Type,
			fields:  map[string]string{},
			indexes: map[string]struct{}{},
			rules:   map[string]string{},
		}
		for _, f := range c.Fields {
			info.fields[f.GetName()] = f.Type()
		}
		for _, idx := range c.Indexes {
			info.indexes[idx] = struct{}{}
		}
		for name, r := range map[string]*string{
			"list": c.ListRule, "view": c.ViewRule, "create": c.CreateRule,
			"update": c.UpdateRule, "delete": c.DeleteRule,
		} {
			if r == nil {
				info.rules[name] = "<null>"
			} else {
				info.rules[name] = *r
			}
		}
		out[c.Name] = info
	}
	return out
}

func report(before, after map[string]colInfo) {
	names := make([]string, 0, len(after))
	for n := range after {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		a := after[n]
		b, existed := before[n]
		if !existed {
			fields := make([]string, 0, len(a.fields))
			for f := range a.fields {
				fields = append(fields, f)
			}
			sort.Strings(fields)
			fmt.Printf("\n+ NEW %-18s (%s) fields=%v indexes=%d\n", n, a.kind, fields, len(a.indexes))
			for _, k := range []string{"list", "view", "create", "update", "delete"} {
				fmt.Printf("      %-6s %s\n", k, a.rules[k])
			}
			continue
		}
		var lines []string
		for f, t := range a.fields {
			if _, ok := b.fields[f]; !ok {
				lines = append(lines, fmt.Sprintf("      + field   %s (%s)", f, t))
			}
		}
		for f := range b.fields {
			if _, ok := a.fields[f]; !ok {
				lines = append(lines, fmt.Sprintf("      - FIELD   %s  <-- DATA LOSS", f))
			}
		}
		for i := range a.indexes {
			if _, ok := b.indexes[i]; !ok {
				lines = append(lines, "      + index   "+i)
			}
		}
		for i := range b.indexes {
			if _, ok := a.indexes[i]; !ok {
				lines = append(lines, "      - index   "+i)
			}
		}
		for _, k := range []string{"list", "view", "create", "update", "delete"} {
			if a.rules[k] != b.rules[k] {
				lines = append(lines, fmt.Sprintf("      ~ %s rule\n          from: %s\n          to:   %s",
					k, b.rules[k], a.rules[k]))
			}
		}
		if len(lines) > 0 {
			sort.Strings(lines)
			fmt.Printf("\n~ %s\n", n)
			for _, l := range lines {
				fmt.Println(l)
			}
		}
	}

	for n := range before {
		if _, ok := after[n]; !ok {
			fmt.Printf("\n- DROPPED %s  <-- DATA LOSS\n", n)
		}
	}
}
