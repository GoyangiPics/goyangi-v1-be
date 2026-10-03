package hooks

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// r2URLFields is the single source of truth for every "contents" field that
// holds an R2 object URL.
//
// Adding a rendition means touching four places: the encoder, the upload switch
// in moveFileToCustomR2Path, THIS list, and the collection schema. Miss this one
// and the objects leak both on delete and on reprocess.
//
// Deliberately excluded: `source`, `discord` and `mirror` are third-party URLs
// that we must never delete, and `file` is the PocketBase-side upload, which
// PocketBase removes itself along with the record's storage prefix.
var r2URLFields = []string{"original", "static", "preview", "sd"}

const (
	// r2DeleteQueueSize bounds how many pending keys we hold in memory. Deleting
	// a 200-item set enqueues up to ~800 keys, so this leaves generous headroom
	// while still being a hard ceiling.
	r2DeleteQueueSize = 4096

	// r2DeleteBatchMax caps one batch so a huge cascade doesn't hold a single
	// filesystem client open indefinitely.
	r2DeleteBatchMax = 512

	// r2DeleteCoalesce is how long the dispatcher waits for more keys before
	// flushing. A cascade delete fires one hook per child, so a short window
	// collapses hundreds of individual batches into a handful.
	r2DeleteCoalesce = 250 * time.Millisecond

	// r2DrainTimeout bounds the best-effort flush at shutdown.
	r2DrainTimeout = 10 * time.Second
)

// r2DeleteQueue carries object KEYS (not URLs) awaiting deletion.
var r2DeleteQueue = make(chan string, r2DeleteQueueSize)

// r2DeleteWorkers is how many concurrent fs.Delete calls one batch runs.
// Previously this was effectively unbounded: one goroutine AND one freshly
// constructed S3 client per URL, with nothing waiting on them, so deleting a
// large set could spawn hundreds of each and lose them all on shutdown.
var r2DeleteWorkers = envInt("MAX_R2_DELETE_JOBS", 8)

// r2UpdateCleanupEnabled gates the update/reprocess cleanup hook. On by default;
// set GOYANGI_R2_UPDATE_CLEANUP=0 to disable it without a redeploy of behaviour
// that touches deletion.
var r2UpdateCleanupEnabled = os.Getenv("GOYANGI_R2_UPDATE_CLEANUP") != "0"

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			return parsed
		}
	}
	return def
}

// r2PublicBase returns R2_PUBLIC_URL without its trailing slash.
func r2PublicBase() string {
	return strings.TrimRight(os.Getenv("R2_PUBLIC_URL"), "/")
}

// r2KeyFromURL converts a stored public URL to its R2 object key. Reports false
// for anything not under R2_PUBLIC_URL — a third-party URL that ended up in one
// of these fields must never be turned into a delete.
func r2KeyFromURL(url string) (string, bool) {
	if url == "" {
		return "", false
	}
	base := r2PublicBase()
	if base == "" {
		return "", false
	}
	key := strings.TrimPrefix(url, base+"/")
	if key == url || key == "" {
		return "", false
	}
	return key, true
}

// r2URLs returns the distinct R2-hosted URLs a record points at.
func r2URLs(record *core.Record) []string {
	seen := make(map[string]struct{}, len(r2URLFields))
	var out []string
	for _, field := range r2URLFields {
		url := record.GetString(field)
		if url == "" {
			continue
		}
		if _, dup := seen[url]; dup {
			continue
		}
		seen[url] = struct{}{}
		out = append(out, url)
	}
	return out
}

// staleR2URLs returns the URLs present in before but not in after — the objects
// an update orphaned.
func staleR2URLs(before, after []string) []string {
	if len(before) == 0 {
		return nil
	}
	kept := make(map[string]struct{}, len(after))
	for _, url := range after {
		kept[url] = struct{}{}
	}
	var stale []string
	for _, url := range before {
		if _, ok := kept[url]; !ok {
			stale = append(stale, url)
		}
	}
	return stale
}

// r2URLStillReferenced reports whether any "contents" row still points at the
// given URL through one of r2URLFields.
//
// Both callers rely on the row's own values no longer matching: the delete hook
// runs after the row is gone, and the update hook only asks about URLs the new
// version has already dropped. A match therefore means a *different* record
// shares the object, which happens because stills historically stored the same
// key in both `original` and `preview`.
func r2URLStillReferenced(app core.App, url string) (bool, error) {
	if url == "" {
		return false, nil
	}
	conds := make([]string, 0, len(r2URLFields))
	for _, field := range r2URLFields {
		conds = append(conds, fmt.Sprintf("`%s` = {:url}", field))
	}
	var count int
	err := app.DB().
		NewQuery("SELECT COUNT(*) FROM `contents` WHERE " + strings.Join(conds, " OR ")).
		Bind(dbx.Params{"url": url}).
		Row(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// enqueueR2DeleteURLs converts URLs to keys, drops any that another record still
// references, and queues the rest.
func enqueueR2DeleteURLs(app core.App, urls []string, recordId string) {
	var keys []string
	for _, url := range urls {
		key, ok := r2KeyFromURL(url)
		if !ok {
			log.Printf("⚠️  R2: %q is not under R2_PUBLIC_URL — skipping delete", url)
			continue
		}
		referenced, err := r2URLStillReferenced(app, url)
		if err != nil {
			// Fail closed: a failed lookup must not delete an object that might
			// still be in use. Leaking bytes is recoverable, deleting live
			// content is not.
			LogUploadWarning(
				fmt.Sprintf("R2: reference check failed for %s, keeping the object: %v", key, err),
				map[string]any{"record": recordId, "key": key},
			)
			continue
		}
		if referenced {
			log.Printf("ℹ️  R2: %s still referenced by another record — keeping", key)
			continue
		}
		keys = append(keys, key)
	}
	enqueueR2DeleteKeys(keys...)
}

// enqueueR2DeleteKeys queues keys for deletion without blocking.
//
// A delete hook runs on a request goroutine (and inside PocketBase's post-commit
// callbacks), so this must never block. On a full queue the key is logged rather
// than dropped silently, so the object can be reclaimed by hand.
func enqueueR2DeleteKeys(keys ...string) {
	for _, key := range keys {
		if key == "" {
			continue
		}
		select {
		case r2DeleteQueue <- key:
		default:
			LogUploadWarning(
				fmt.Sprintf("R2: delete queue full, leaking object %s", key),
				map[string]any{"key": key},
			)
		}
	}
}

// startR2DeleteDispatcher drains the queue in coalesced batches for the life of
// the process.
func startR2DeleteDispatcher(app *pocketbase.PocketBase) {
	go func() {
		for first := range r2DeleteQueue {
			batch := []string{first}
			timer := time.NewTimer(r2DeleteCoalesce)
		collect:
			for len(batch) < r2DeleteBatchMax {
				select {
				case key := <-r2DeleteQueue:
					batch = append(batch, key)
				case <-timer.C:
					break collect
				}
			}
			timer.Stop()
			deleteR2Batch(app, batch)
		}
	}()
}

// drainR2DeleteQueue makes a bounded best-effort attempt to flush whatever is
// still queued at shutdown.
//
// Anything left over is leaked. That is a known gap: a hard crash between an R2
// upload and its enqueue leaks too, and the only complete answer is a
// reconciliation sweep over the bucket (see README, "Object lifecycle").
func drainR2DeleteQueue(app *pocketbase.PocketBase) {
	deadline := time.Now().Add(r2DrainTimeout)
	var pending []string

	for len(pending) < r2DeleteQueueSize && time.Now().Before(deadline) {
		select {
		case key := <-r2DeleteQueue:
			pending = append(pending, key)
			continue
		default:
		}
		break // queue empty
	}

	if len(pending) == 0 {
		return
	}
	log.Printf("🧹 R2: draining %d queued delete(s) before shutdown", len(pending))
	deleteR2Batch(app, pending)
}

// deleteR2Batch removes a batch of keys using ONE filesystem client, bounded by
// r2DeleteWorkers concurrent deletes.
func deleteR2Batch(app *pocketbase.PocketBase, keys []string) {
	seen := make(map[string]struct{}, len(keys))
	uniq := make([]string, 0, len(keys))
	for _, key := range keys {
		if key == "" {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		uniq = append(uniq, key)
	}
	if len(uniq) == 0 {
		return
	}

	fs, err := app.NewFilesystem()
	if err != nil {
		LogUploadError(
			fmt.Sprintf("R2: filesystem init failed, leaking %d object(s): %v", len(uniq), err),
			map[string]any{"keys": uniq},
		)
		return
	}
	defer fs.Close()

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed int
		sem    = make(chan struct{}, r2DeleteWorkers)
	)
	for _, key := range uniq {
		wg.Add(1)
		sem <- struct{}{}
		go func(key string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fs.Delete(key); err != nil {
				mu.Lock()
				failed++
				mu.Unlock()
				log.Printf("⚠️  R2: could not delete %s: %v", key, err)
			}
		}(key)
	}
	wg.Wait()

	if failed > 0 {
		log.Printf("🧹 R2: deleted %d/%d object(s), %d failed", len(uniq)-failed, len(uniq), failed)
	} else {
		log.Printf("🧹 R2: deleted %d object(s)", len(uniq))
	}
}
