package hooks

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/tools/types"
)

// withFakeEncoder routes startEncode to `run` against the fixture's app and
// waits for every queued run to finish before the test reads the results.
func withFakeEncoder(t *testing.T, f *fixture, run func(id string)) *sync.WaitGroup {
	t.Helper()
	if err := ensureEncodeFields(f.app); err != nil {
		t.Fatalf("fields: %v", err)
	}
	var wg sync.WaitGroup
	prev := postEncoder
	postEncoder = &encoder{app: f.app, run: run, done: func(string) { wg.Done() }}
	t.Cleanup(func() { postEncoder = prev })
	return &wg
}

// waitFor fails the test instead of hanging it when a queued run never comes.
func waitFor(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a queued encode never ran")
	}
}

// stuckPost is a post whose encode never finished: source present, no preview.
func stuckPost(t *testing.T, f *fixture, owner string, attempts int) string {
	t.Helper()
	id := f.post(t, owner, "")
	if _, err := f.app.DB().NewQuery(
		"UPDATE {{contents}} SET [[file]] = 'source.mp4', [[preview]] = '', [[encodeAttempts]] = {:n} WHERE [[id]] = {:id}",
	).Bind(dbx.Params{"id": id, "n": attempts}).Execute(); err != nil {
		t.Fatalf("stick: %v", err)
	}
	return id
}

func TestRetryRecordsTheOutcome(t *testing.T) {
	f := newFixture(t)
	succeed := true
	wg := withFakeEncoder(t, f, func(id string) {
		if succeed {
			_, _ = f.app.DB().NewQuery("UPDATE {{contents}} SET [[preview]] = 'p.avif' WHERE [[id]] = {:id}").
				Bind(dbx.Params{"id": id}).Execute()
		}
	})
	failing := stuckPost(t, f, "u1", 0)

	succeed = false
	wg.Add(1)
	f.do(t, "owner retries", http.MethodPost, "/api/contents/"+failing+"/reprocess", "", "u1", 202)
	waitFor(t, wg)
	r := f.record(t, "contents", failing)
	if r.GetInt("encodeAttempts") != 1 || r.GetString("encodeError") == "" {
		t.Errorf("a failed run should count and say why: attempts=%d error=%q", r.GetInt("encodeAttempts"), r.GetString("encodeError"))
	}

	succeed = true
	wg.Add(1)
	f.do(t, "second try works", http.MethodPost, "/api/contents/"+failing+"/reprocess", "", "u1", 202)
	waitFor(t, wg)
	r = f.record(t, "contents", failing)
	if r.GetString("encodeError") != "" || r.GetInt("encodeAttempts") != 2 {
		t.Errorf("a successful run clears the error: attempts=%d error=%q", r.GetInt("encodeAttempts"), r.GetString("encodeError"))
	}
	f.do(t, "nothing left to do", http.MethodPost, "/api/contents/"+failing+"/reprocess", "", "u1", 400)
}

func TestRetryPermissionsAndLimits(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	wg := withFakeEncoder(t, f, func(string) { <-release })
	p := stuckPost(t, f, "u1", 0)
	worn := stuckPost(t, f, "u1", maxEncodeAttempts)

	f.do(t, "someone else's post", http.MethodPost, "/api/contents/"+p+"/reprocess", "", "u2", 403)
	f.do(t, "out of retries", http.MethodPost, "/api/contents/"+worn+"/reprocess", "", "u1", 409)

	wg.Add(1)
	f.do(t, "queued", http.MethodPost, "/api/contents/"+p+"/reprocess", "", "u1", 202)
	f.do(t, "already processing", http.MethodPost, "/api/contents/"+p+"/reprocess", "", "admin", 409)

	wg.Add(1)
	f.do(t, "admin can always retry", http.MethodPost, "/api/contents/"+worn+"/reprocess", "", "admin", 202)
	close(release)
	waitFor(t, wg)
}

func TestBootRequeuesInterruptedPosts(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	ran := map[string]bool{}
	wg := withFakeEncoder(t, f, func(id string) {
		mu.Lock()
		ran[id] = true
		mu.Unlock()
	})

	old := types.NowDateTime().Add(-time.Hour).String()
	backdate := func(id string) {
		_, _ = f.app.DB().NewQuery("UPDATE {{contents}} SET [[created]] = {:c} WHERE [[id]] = {:id}").
			Bind(dbx.Params{"id": id, "c": old}).Execute()
	}
	interrupted := stuckPost(t, f, "u1", 0)
	backdate(interrupted)
	exhausted := stuckPost(t, f, "u1", maxEncodeAttempts)
	backdate(exhausted)
	recent := stuckPost(t, f, "u1", 0) // maybe still running in the old process

	wg.Add(1)
	requeueInterrupted(f.app)
	waitFor(t, wg)

	if !ran[interrupted] || ran[exhausted] || ran[recent] {
		t.Errorf("ran = %v; want only the interrupted one (%s)", ran, interrupted)
	}
}
