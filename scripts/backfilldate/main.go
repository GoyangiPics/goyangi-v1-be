// Command backfilldate gives every undated content and set its upload time as
// its date.
//
// New and edited records get this from the save hooks in hooks/content_date.go.
// Everything older than those hooks may have no date, and PocketBase sorts empty
// values first — so on the site, "oldest by actual date" opens on every undated
// item, and an actual-date range leaves them out entirely.
//
// Like backfilldims, it talks to a RUNNING PocketBase over HTTP rather than
// opening pb_data, so it can be pointed at the deployed instance from a laptop
// with no downtime and no second process on the SQLite file.
//
// Dry run by default: nothing is written without -commit.
//
//	# see how many it would date
//	go run ./scripts/backfilldate -url https://api.example.com
//
//	# do it
//	go run ./scripts/backfilldate -url https://api.example.com -commit
//
// Credentials come from PB_ADMIN_EMAIL / PB_ADMIN_PASSWORD, or -email/-password.
// Prefer the environment: flags are visible in the process list.
//
// Each write is a normal record update: it fires the update hooks and a realtime
// event per record, and bumps `updated`. The hooks are harmless here — the R2
// stale-URL cleanup finds nothing, since no URL changes — and nothing in the
// frontend sorts by `updated`. -delay paces the writes if realtime subscribers
// need going easy on. Re-running is safe: it only ever picks up what is still
// undated.
package main

import (
	"bytes"
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
	"sync"
	"sync/atomic"
	"time"
)

// collections the backfill covers, in the order it runs them.
var collections = []string{"contents", "contents_sets"}

type record struct {
	Id      string `json:"id"`
	Created string `json:"created"`
}

type listResponse struct {
	Page       int      `json:"page"`
	TotalPages int      `json:"totalPages"`
	Items      []record `json:"items"`
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
	commit := flag.Bool("commit", false, "actually write the dates instead of reporting them")
	only := flag.String("collection", "", "only this collection (contents|contents_sets)")
	limit := flag.Int("limit", 0, "stop after this many records per collection (0 = all)")
	concurrency := flag.Int("concurrency", 4, "parallel writes")
	delay := flag.Duration("delay", 0, "pause after each write, to go easy on realtime subscribers")
	flag.Parse()

	if *email == "" || *password == "" {
		log.Fatal("superuser credentials required: set PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD, or pass -email/-password")
	}
	if *concurrency < 1 {
		log.Fatal("-concurrency must be at least 1")
	}
	targets := collections
	if *only != "" {
		if *only != "contents" && *only != "contents_sets" {
			log.Fatalf("-collection must be contents or contents_sets, not %q", *only)
		}
		targets = []string{*only}
	}

	c := &client{
		baseURL: strings.TrimRight(*baseURL, "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
	if err := c.authenticate(*email, *password); err != nil {
		log.Fatalf("auth failed: %v", err)
	}
	log.Printf("🔑 authenticated against %s", c.baseURL)
	if !*commit {
		log.Printf("🔍 DRY RUN — nothing will be written. Pass -commit to apply.")
	}

	var totalFailed int64
	for _, collection := range targets {
		pending, err := c.fetchUndated(collection, *limit)
		if err != nil {
			log.Fatalf("listing %s: %v", collection, err)
		}
		log.Printf("📋 %s: %d undated record(s)", collection, len(pending))
		if len(pending) == 0 || !*commit {
			continue
		}

		var updated, failed atomic.Int64
		jobs := make(chan record)
		var wg sync.WaitGroup
		var writeMu sync.Mutex

		for i := 0; i < *concurrency; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for r := range jobs {
					var err error
					if *delay > 0 {
						// Serialised so -delay paces the writes as a whole
						// rather than each worker independently.
						writeMu.Lock()
						err = c.patchDate(collection, r)
						time.Sleep(*delay)
						writeMu.Unlock()
					} else {
						err = c.patchDate(collection, r)
					}
					if err != nil {
						failed.Add(1)
						log.Printf("❌ %s/%s: %v", collection, r.Id, err)
						continue
					}
					if n := updated.Add(1); n%500 == 0 {
						log.Printf("… %s: %d/%d", collection, n, len(pending))
					}
				}
			}()
		}
		for _, r := range pending {
			jobs <- r
		}
		close(jobs)
		wg.Wait()

		log.Printf("── %s: %d dated, %d failed (of %d)", collection, updated.Load(), failed.Load(), len(pending))
		totalFailed += failed.Load()
	}

	if totalFailed > 0 {
		// Non-zero so a partial run is visible to whatever invoked it. Safe to
		// leave: the failures stay undated and a re-run picks up exactly those.
		os.Exit(1)
	}
}

func (c *client) authenticate(email, password string) error {
	body, err := json.Marshal(map[string]string{"identity": email, "password": password})
	if err != nil {
		return err
	}
	resp, err := c.http.Post(
		c.baseURL+"/api/collections/_superusers/auth-with-password",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}

	var out struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return err
	}
	if out.Token == "" {
		return fmt.Errorf("no token in response")
	}
	c.token = out.Token
	return nil
}

// fetchUndated lists every record in collection with no date.
//
// Collected up front rather than paged while writing: each write takes a record
// out of the filter, so paging through a shrinking result would skip records.
func (c *client) fetchUndated(collection string, limit int) ([]record, error) {
	var all []record
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("perPage", "500")
		q.Set("filter", `date = ""`)
		q.Set("fields", "id,created")
		// Oldest first, so an interrupted run has covered a contiguous stretch.
		q.Set("sort", "created")

		var out listResponse
		if err := c.getJSON("/api/collections/"+collection+"/records?"+q.Encode(), &out); err != nil {
			return nil, err
		}

		all = append(all, out.Items...)
		if limit > 0 && len(all) >= limit {
			return all[:limit], nil
		}
		if page >= out.TotalPages || len(out.Items) == 0 {
			return all, nil
		}
	}
}

func (c *client) getJSON(path string, into any) error {
	req, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// patchDate sets the record's date to its upload time — stated outright rather
// than left to the update hook, so the script does the same thing against a
// server that predates the hook.
func (c *client) patchDate(collection string, r record) error {
	if r.Created == "" {
		return fmt.Errorf("no created time to use")
	}
	body, err := json.Marshal(map[string]string{"date": r.Created})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(
		http.MethodPatch,
		c.baseURL+"/api/collections/"+collection+"/records/"+url.PathEscape(r.Id),
		bytes.NewReader(body),
	)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}
