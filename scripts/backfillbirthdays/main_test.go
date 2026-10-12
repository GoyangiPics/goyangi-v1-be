package main

import (
	"encoding/json"
	"testing"
)

func strp(s string) *string { return &s }

func TestMakePlan(t *testing.T) {
	idols := []idol{
		{Id: "a", Name: "Wonyoung"},
		{Id: "b", Name: "Chaewon", Birthday: "2000-08-01 00:00:00.000Z"},
		{Id: "c", Name: "Yujin", Birthday: "2003-09-01 00:00:00.000Z"},
		{Id: "d", Name: "Karina"},
	}
	entries := []entry{
		{Id: "a", Name: "wonyoung", Birthday: strp("2004-08-31")}, // case-insensitive name → write
		{Id: "b", Name: "Chaewon", Birthday: strp("2000-08-01")},  // same date → unchanged
		{Id: "c", Name: "Yujin", Birthday: strp("2003-09-03")},    // set differently → conflict
		{Id: "d", Name: "Winter", Birthday: strp("2001-01-01")},   // id collides, wrong idol → renamed
		{Id: "x", Name: "Ningning", Birthday: strp("2002-10-23")}, // not in db → missing
		{Id: "a", Name: "Wonyoung", Birthday: strp("2004-02-30")}, // impossible date → invalid
		{Id: "d", Name: "Karina", Birthday: nil},                  // not found → ignored
	}

	p := makePlan(entries, idols, false)
	if len(p.writes) != 1 || p.writes[0].idol.Id != "a" || p.writes[0].birthday != "2004-08-31" {
		t.Errorf("writes = %+v", p.writes)
	}
	if p.unchanged != 1 {
		t.Errorf("unchanged = %d, want 1", p.unchanged)
	}
	if len(p.conflicts) != 1 || len(p.renamed) != 1 || len(p.missing) != 1 || len(p.invalid) != 1 {
		t.Errorf("conflicts=%v renamed=%v missing=%v invalid=%v", p.conflicts, p.renamed, p.missing, p.invalid)
	}

	p = makePlan(entries, idols, true)
	if len(p.writes) != 2 || len(p.conflicts) != 0 {
		t.Errorf("with overwrite: writes=%+v conflicts=%v", p.writes, p.conflicts)
	}
}

// The embedded list must parse, and every id must be unique — a duplicate
// would make the second row silently win.
func TestBirthdaysJSON(t *testing.T) {
	var entries []entry
	if err := json.Unmarshal(birthdaysJSON, &entries); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if seen[e.Id] {
			t.Errorf("duplicate id %s (%s)", e.Id, e.Name)
		}
		seen[e.Id] = true
	}
}
