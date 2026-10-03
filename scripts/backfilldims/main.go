// Command backfilldims fills in contents.width/height for records uploaded
// before the transcode pipeline started storing them.
//
// New uploads get their dimensions from setRecordDimensions (see
// hooks/dimensions.go). Everything already in the archive has nothing, and the
// frontend falls back to measuring the media on load for those — which is what
// makes cards jump. This walks the archive and measures them once.
//
// It talks to a RUNNING PocketBase over HTTP rather than opening pb_data
// directly, so it can be pointed at the deployed instance from a laptop with no
// downtime and no second process on the SQLite file. ffprobe reads dimensions
// straight from the R2 public URL over HTTPS — for an MP4 that is a few range
// requests for the moov box, not a download of the whole object.
//
// Dry run by default, matching schemacheck: nothing is written without -commit.
//
//	# see what it would do
//	go run ./scripts/backfilldims -url https://api.example.com
//
//	# the easy filetypes first, then the rest
//	go run ./scripts/backfilldims -url https://api.example.com -filetype gif -commit
//	go run ./scripts/backfilldims -url https://api.example.com -commit
//
// Credentials come from PB_ADMIN_EMAIL / PB_ADMIN_PASSWORD, or -email/-password.
// Prefer the environment: flags are visible in the process list.
//
// Two things to know before running it against production. Each write is a
// normal record update, so it fires the contents update hooks and a realtime
// event per record — harmless (no R2 URL changes, so the stale-URL cleanup finds
// nothing to delete) but chatty, which is what -delay and a modest -concurrency
// are for. And it bumps `updated`; nothing in the frontend sorts by it, every
// listing sorts by -created or -likes:length.
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
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Stickers are always 200×200 — generateStickerAVIF crops to a square and scales
// to a fixed box, so there is nothing to probe. Kept in sync with
// hooks/dimensions.go's stickerDimensions by this comment and nothing else,
// which is fine for a one-off: a wrong value here is one re-run away from fixed.
const stickerDimensions = 200

// probeTimeout bounds a single ffprobe. Generous compared to the local probe in
// the pipeline, because this one is reading over the network.
const probeTimeout = 60 * time.Second

type record struct {
	Id       string `json:"id"`
	Filetype string `json:"filetype"`
	Original string `json:"original"`
	Preview  string `json:"preview"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
}

type listResponse struct {
	Page       int      `json:"page"`
	PerPage    int      `json:"perPage"`
	TotalItems int      `json:"totalItems"`
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
	commit := flag.Bool("commit", false, "actually write the dimensions instead of reporting them")
	filetype := flag.String("filetype", "", "only this filetype (gif|video|image|sticker)")
	limit := flag.Int("limit", 0, "stop after this many records (0 = all)")
	concurrency := flag.Int("concurrency", 4, "parallel probes")
	delay := flag.Duration("delay", 0, "pause after each write, to go easy on realtime subscribers")
	flag.Parse()

	if *email == "" || *password == "" {
		log.Fatal("superuser credentials required: set PB_ADMIN_EMAIL and PB_ADMIN_PASSWORD, or pass -email/-password")
	}
	if *concurrency < 1 {
		log.Fatal("-concurrency must be at least 1")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		log.Fatal("ffprobe not found in PATH — this script measures media with it")
	}

	c := &client{
		baseURL: strings.TrimRight(*baseURL, "/"),
		http:    &http.Client{Timeout: 60 * time.Second},
	}
	if err := c.authenticate(*email, *password); err != nil {
		log.Fatalf("auth failed: %v", err)
	}
	log.Printf("🔑 authenticated against %s", c.baseURL)

	pending, err := c.fetchPending(*filetype, *limit)
	if err != nil {
		log.Fatalf("listing records: %v", err)
	}
	log.Printf("📋 %d record(s) with no dimensions", len(pending))
	if len(pending) == 0 {
		return
	}
	if !*commit {
		log.Printf("🔍 DRY RUN — nothing will be written. Pass -commit to apply.")
	}

	var updated, skipped, failed atomic.Int64

	// A plain worker pool over a channel: the work is one independent probe per
	// record, and ffprobe over the network is latency-bound, so a few in flight
	// is a large speedup over sequential.
	jobs := make(chan record)
	var wg sync.WaitGroup
	var writeMu sync.Mutex

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				w, h, err := measure(r)
				if err != nil {
					failed.Add(1)
					log.Printf("❌ %s (%s): %v", r.Id, r.Filetype, err)
					continue
				}

				if !*commit {
					skipped.Add(1)
					log.Printf("🔍 %s (%s): would set %dx%d", r.Id, r.Filetype, w, h)
					continue
				}

				// Serialised so -delay actually paces the writes rather than
				// pacing each worker independently.
				writeMu.Lock()
				err = c.patchDimensions(r.Id, w, h)
				if err == nil && *delay > 0 {
					time.Sleep(*delay)
				}
				writeMu.Unlock()

				if err != nil {
					failed.Add(1)
					log.Printf("❌ %s: write failed: %v", r.Id, err)
					continue
				}
				updated.Add(1)
				log.Printf("✅ %s (%s): %dx%d", r.Id, r.Filetype, w, h)
			}
		}()
	}

	for _, r := range pending {
		jobs <- r
	}
	close(jobs)
	wg.Wait()

	log.Printf("── done: %d updated, %d reported, %d failed (of %d)",
		updated.Load(), skipped.Load(), failed.Load(), len(pending))
	if failed.Load() > 0 {
		// A non-zero exit makes a partial run visible to whatever invoked it.
		// Failures are safe to leave: the records keep no dimensions, the frontend
		// keeps measuring them, and a re-run picks up exactly what is still
		// missing.
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

// fetchPending pages through every record that has no usable dimensions yet.
//
// `width = null` covers rows that predate the field; `width = 0` covers rows a
// failed probe left behind, so a re-run retries them.
func (c *client) fetchPending(filetype string, limit int) ([]record, error) {
	filter := "(width = null || width = 0 || height = null || height = 0)"
	if filetype != "" {
		filter += fmt.Sprintf(" && filetype = %q", filetype)
	}

	var all []record
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("page", strconv.Itoa(page))
		q.Set("perPage", "200")
		q.Set("filter", filter)
		q.Set("fields", "id,filetype,original,preview,width,height")
		// Oldest first, so an interrupted run has covered a contiguous stretch of
		// the archive rather than a random sample of it.
		q.Set("sort", "created")
		// skipTotal would save a count query, but the total is the only progress
		// signal a long run has.
		q.Set("skipTotal", "0")

		var out listResponse
		if err := c.getJSON("/api/collections/contents/records?"+q.Encode(), &out); err != nil {
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

func (c *client) patchDimensions(id string, w, h int) error {
	body, err := json.Marshal(map[string]int{"width": w, "height": h})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(
		http.MethodPatch,
		c.baseURL+"/api/collections/contents/records/"+url.PathEscape(id),
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

// measure returns the dimensions to store for one record.
func measure(r record) (int, int, error) {
	// Nothing to probe: the sticker pipeline's crop+scale makes the output a
	// fixed square, and it is also the one filetype whose source ratio is
	// discarded, so measuring the file would be no more correct than this.
	if r.Filetype == "sticker" {
		return stickerDimensions, stickerDimensions, nil
	}

	// `original` is the canonical rendition; `preview` is the fallback for a
	// record whose original never landed. Both preserve the display aspect.
	target := r.Original
	if target == "" {
		target = r.Preview
	}
	if target == "" {
		return 0, 0, fmt.Errorf("no original or preview URL to measure")
	}

	return probeURL(target)
}

// probeURL runs ffprobe against a URL and parses its dimensions.
//
// This is deliberately a copy of hooks.probeDimensions' probe rather than an
// import: that one writes bytes to a temp file, which would mean downloading
// every object in the archive in full.
func probeURL(target string) (int, int, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0",
		target,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return 0, 0, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		if err != nil {
			return 0, 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
	case <-time.After(probeTimeout):
		_ = cmd.Process.Kill()
		return 0, 0, fmt.Errorf("ffprobe timed out after %s", probeTimeout)
	}

	return parseDimensions(stdout.String())
}

// parseDimensions mirrors hooks.parseDimensions — see the notes there on why
// ffprobe's csv output needs more than a Split.
func parseDimensions(out string) (int, int, error) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 2 {
			continue
		}
		w, wErr := strconv.Atoi(strings.TrimSpace(fields[0]))
		h, hErr := strconv.Atoi(strings.TrimSpace(fields[1]))
		if wErr != nil || hErr != nil || w <= 0 || h <= 0 {
			continue
		}
		return w, h, nil
	}
	return 0, 0, fmt.Errorf("no usable dimensions in probe output %q", out)
}
