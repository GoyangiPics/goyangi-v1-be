// Command backfillbirthdays fills groups_idols.birthday from birthdays.json, a
// researched list kept next to it with a source for every date.
//
// The list is keyed by record id and carries each idol's name as a guard: a
// row whose id exists but whose name doesn't match is reported and skipped, so
// pointing the script at a different database (a dev copy whose ids happen to
// collide) can't stamp one idol's birthday onto another.
//
// It never overwrites a birthday that is already set — those were entered by
// hand in the admin UI and win over the research — unless -overwrite is given.
// Disagreements are listed either way, so they can be looked at.
//
// Like backfilldate, it talks to a RUNNING PocketBase over HTTP rather than
// opening pb_data, so it can be pointed at the deployed instance from a laptop.
//
// Dry run by default: nothing is written without -commit.
//
//	go run ./scripts/backfillbirthdays -url https://api.goyangi.pics
//	go run ./scripts/backfillbirthdays -url https://api.goyangi.pics -commit
//
// Credentials come from PB_ADMIN_EMAIL / PB_ADMIN_PASSWORD, or -email/-password.
// Prefer the environment: flags are visible in the process list. Re-running is
// safe: idols already carrying the listed date are skipped.
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed birthdays.json
var birthdaysJSON []byte

// entry is one row of birthdays.json. Birthday is YYYY-MM-DD, or null where the
// research found nothing reliable (the note says why).
type entry struct {
	Id       string  `json:"id"`
	Name     string  `json:"name"`
	Group    *string `json:"group"`
	Birthday *string `json:"birthday"`
	Source   string  `json:"source"`
	Note     string  `json:"note,omitempty"`
}

type idol struct {
	Id       string `json:"id"`
	Name     string `json:"name"`
	Birthday string `json:"birthday"`
}

// pbDate is the layout PocketBase emits for date fields; a birthday is stored
// as midnight UTC so its first ten characters are the calendar date, which is
// all the frontend reads.
const pbDate = "2006-01-02 15:04:05.000Z"

type change struct {
	idol     idol
	birthday string // YYYY-MM-DD
}

type plan struct {
	writes    []change
	unchanged int
	missing   []entry  // id not in the database
	renamed   []string // id found, name differs
	conflicts []string // a different birthday is already set
	invalid   []string // unparseable date in the list
}

// makePlan works out what to write. Pure, so the guards are testable without a
// server.
func makePlan(entries []entry, idols []idol, overwrite bool) plan {
	byID := make(map[string]idol, len(idols))
	for _, i := range idols {
		byID[i.Id] = i
	}

	var p plan
	for _, e := range entries {
		if e.Birthday == nil {
			continue
		}
		if _, err := time.Parse(time.DateOnly, *e.Birthday); err != nil {
			p.invalid = append(p.invalid, fmt.Sprintf("%s (%s): %q", e.Name, e.Id, *e.Birthday))
			continue
		}
		current, ok := byID[e.Id]
		if !ok {
			p.missing = append(p.missing, e)
			continue
		}
		if !strings.EqualFold(current.Name, e.Name) {
			p.renamed = append(p.renamed, fmt.Sprintf("%s: list says %q, database says %q", e.Id, e.Name, current.Name))
			continue
		}
		existing := dateOnly(current.Birthday)
		switch {
		case existing == *e.Birthday:
			p.unchanged++
		case existing != "" && !overwrite:
			p.conflicts = append(p.conflicts, fmt.Sprintf("%s (%s): set to %s, list says %s", e.Name, e.Id, existing, *e.Birthday))
		default:
			p.writes = append(p.writes, change{idol: current, birthday: *e.Birthday})
		}
	}
	return p
}

func dateOnly(pb string) string {
	if len(pb) < len(time.DateOnly) {
		return ""
	}
	return pb[:len(time.DateOnly)]
}

type client struct {
	baseURL string
	token   string
	http    *http.Client
}

func main() {
	baseURL := flag.String("url", "http://127.0.0.1:8090", "PocketBase base URL")
	email := flag.String("email", os.Getenv("PB_ADMIN_EMAIL"), "superuser email (prefer PB_ADMIN_EMAIL)")
	password := flag.String("password", os.Getenv("PB_ADMIN_PASSWORD"), "superuser password (prefer PB_ADMIN_PASSWORD)")
	commit := flag.Bool("commit", false, "actually write the birthdays instead of reporting them")
	overwrite := flag.Bool("overwrite", false, "replace birthdays that are already set to a different date")
	flag.Parse()

	if *email == "" || *password == "" {
		log.Fatal("superuser credentials required: set PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD, or pass -email/-password")
	}

	var entries []entry
	if err := json.Unmarshal(birthdaysJSON, &entries); err != nil {
		log.Fatalf("birthdays.json: %v", err)
	}

	c := &client{
		baseURL: strings.TrimRight(*baseURL, "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
	if err := c.authenticate(*email, *password); err != nil {
		log.Fatalf("auth failed: %v", err)
	}
	log.Printf("🔑 authenticated against %s", c.baseURL)

	// PocketBase ignores unknown fields on update, so against a server whose
	// schema predates the field every write would "succeed" and store nothing.
	if err := c.requireBirthdayField(); err != nil {
		log.Fatal(err)
	}

	idols, err := c.listIdols()
	if err != nil {
		log.Fatalf("listing groups_idols: %v", err)
	}
	p := makePlan(entries, idols, *overwrite)

	for _, s := range p.invalid {
		log.Printf("⚠️  invalid date, skipped: %s", s)
	}
	for _, e := range p.missing {
		log.Printf("⚠️  not in this database, skipped: %s (%s)", e.Name, e.Id)
	}
	for _, s := range p.renamed {
		log.Printf("⚠️  name mismatch, skipped: %s", s)
	}
	for _, s := range p.conflicts {
		log.Printf("⚠️  already set, kept (pass -overwrite to replace): %s", s)
	}
	log.Printf("📋 %d to write, %d already correct, %d in the list with no date",
		len(p.writes), p.unchanged, countUndated(entries))

	if !*commit {
		for _, w := range p.writes {
			log.Printf("   %s → %s", w.idol.Name, w.birthday)
		}
		log.Printf("🔍 DRY RUN — nothing was written. Pass -commit to apply.")
		return
	}

	failed := 0
	for _, w := range p.writes {
		if err := c.patchBirthday(w); err != nil {
			failed++
			log.Printf("❌ %s (%s): %v", w.idol.Name, w.idol.Id, err)
		}
	}
	log.Printf("── %d written, %d failed", len(p.writes)-failed, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func countUndated(entries []entry) int {
	n := 0
	for _, e := range entries {
		if e.Birthday == nil {
			n++
		}
	}
	return n
}

func (c *client) authenticate(email, password string) error {
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(http.MethodPost, "/api/collections/_superusers/auth-with-password",
		map[string]string{"identity": email, "password": password}, &out); err != nil {
		return err
	}
	if out.Token == "" {
		return fmt.Errorf("no token in response")
	}
	c.token = out.Token
	return nil
}

func (c *client) requireBirthdayField() error {
	var col struct {
		Fields []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"fields"`
	}
	if err := c.do(http.MethodGet, "/api/collections/groups_idols", nil, &col); err != nil {
		return fmt.Errorf("reading the groups_idols schema: %w", err)
	}
	for _, f := range col.Fields {
		if f.Name == "birthday" {
			if f.Type != "date" {
				return fmt.Errorf("groups_idols.birthday is a %s field, expected date", f.Type)
			}
			return nil
		}
	}
	return fmt.Errorf("groups_idols has no birthday field yet: add it (type date) before running this")
}

func (c *client) listIdols() ([]idol, error) {
	var all []idol
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("perPage", "500")
		q.Set("fields", "id,name,birthday")
		var out struct {
			TotalPages int    `json:"totalPages"`
			Items      []idol `json:"items"`
		}
		if err := c.do(http.MethodGet, "/api/collections/groups_idols/records?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Items...)
		if page >= out.TotalPages || len(out.Items) == 0 {
			return all, nil
		}
	}
}

func (c *client) patchBirthday(w change) error {
	day, _ := time.Parse(time.DateOnly, w.birthday) // validated in makePlan
	return c.do(http.MethodPatch, "/api/collections/groups_idols/records/"+url.PathEscape(w.idol.Id),
		map[string]string{"birthday": day.UTC().Format(pbDate)}, nil)
}

// do sends one JSON request and decodes the JSON response into out (if non-nil).
func (c *client) do(method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}
