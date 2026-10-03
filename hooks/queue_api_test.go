package hooks

import (
	"sync"
	"testing"
)

// resetQueueCounters puts the package-level counters back to zero. The counters
// are process-global, so every test here has to leave them clean or it poisons
// the next one.
func resetQueueCounters(t *testing.T) {
	t.Helper()
	queueTotal.Store(0)
	queueActive.Store(0)
	for _, c := range queueByKind {
		c.Store(0)
	}
}

func TestQueueKind(t *testing.T) {
	tests := []struct{ in, want string }{
		{"gif", "gif"},
		{"video", "video"},
		{"image", "image"},
		{"sticker", "sticker"},
		// filetype is a client-supplied select field on a collection any uploader
		// can write to, so unknown values must fall into a fixed bucket rather
		// than growing the map.
		{"", "other"},
		{"exe", "other"},
		{"../../etc/passwd", "other"},
	}
	for _, tt := range tests {
		if got := queueKind(tt.in); got != tt.want {
			t.Errorf("queueKind(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestQueueSnapshotCountsWaitingAndActive(t *testing.T) {
	resetQueueCounters(t)
	defer resetQueueCounters(t)

	// The case the whole feature exists for: 30 jobs committed to, one of them
	// actually encoding. len(processSem) would have said 1.
	releases := make([]func(), 0, 30)
	for i := 0; i < 28; i++ {
		releases = append(releases, enqueueJob("gif"))
	}
	for i := 0; i < 2; i++ {
		releases = append(releases, enqueueJob("video"))
	}
	releaseActive := beginActive()

	snap := queueSnapshot()
	if snap.Total != 30 {
		t.Errorf("Total = %d, want 30", snap.Total)
	}
	if snap.Active != 1 {
		t.Errorf("Active = %d, want 1", snap.Active)
	}
	if snap.Waiting != 29 {
		t.Errorf("Waiting = %d, want 29", snap.Waiting)
	}
	if snap.ByKind["gif"] != 28 || snap.ByKind["video"] != 2 {
		t.Errorf("ByKind = %v, want gif:28 video:2", snap.ByKind)
	}
	// Zero buckets are omitted so the client doesn't render "0 stickers".
	if _, present := snap.ByKind["sticker"]; present {
		t.Errorf("ByKind = %v, want no empty buckets", snap.ByKind)
	}

	releaseActive()
	for _, release := range releases {
		release()
	}

	if snap := queueSnapshot(); snap.Total != 0 || snap.Active != 0 || snap.Waiting != 0 {
		t.Errorf("after draining: %+v, want all zero", snap)
	}
	if len(queueSnapshot().ByKind) != 0 {
		t.Errorf("after draining: ByKind = %v, want empty", queueSnapshot().ByKind)
	}
}

func TestQueueReleaseIsIdempotent(t *testing.T) {
	resetQueueCounters(t)
	defer resetQueueCounters(t)

	// The counters have no floor, so a double decrement would report a negative
	// queue — or hide a real backlog — for the rest of the process's life.
	release := enqueueJob("gif")
	release()
	release()
	release()

	releaseActive := beginActive()
	releaseActive()
	releaseActive()

	snap := queueSnapshot()
	if snap.Total != 0 || snap.Active != 0 || snap.Waiting != 0 {
		t.Errorf("got %+v, want all zero", snap)
	}
	if got := queueByKind["gif"].Load(); got != 0 {
		t.Errorf("gif bucket = %d, want 0", got)
	}
}

func TestQueueSnapshotNeverReportsNegativeWaiting(t *testing.T) {
	resetQueueCounters(t)
	defer resetQueueCounters(t)

	// Total and Active are independent atomics read one after the other, so a
	// job that finishes between the two loads must make the snapshot stale, not
	// inconsistent. Simulated here by leaving Active high with Total already at
	// zero, which is exactly what that interleaving looks like.
	queueActive.Store(3)

	snap := queueSnapshot()
	if snap.Waiting < 0 {
		t.Errorf("Waiting = %d, want >= 0", snap.Waiting)
	}
	if snap.Active > snap.Total {
		t.Errorf("Active = %d > Total = %d", snap.Active, snap.Total)
	}
}

func TestQueueCountersAreRaceFree(t *testing.T) {
	resetQueueCounters(t)
	defer resetQueueCounters(t)

	// Enqueue happens on the request goroutine and release on the worker, so the
	// counters are genuinely concurrent. Meaningful under -race.
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := enqueueJob("gif")
			releaseActive := beginActive()
			_ = queueSnapshot()
			releaseActive()
			release()
		}()
	}
	wg.Wait()

	if snap := queueSnapshot(); snap.Total != 0 || snap.Active != 0 {
		t.Errorf("got %+v, want all zero", snap)
	}
}
