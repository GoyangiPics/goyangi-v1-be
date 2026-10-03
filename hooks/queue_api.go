package hooks

import (
	"sync"
	"sync/atomic"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// How deep the encode queue is, and what is in it.
//
// The queue is not a data structure anywhere — it is one goroutine per pending
// job, all blocked on processSem (see r2.go). len(processSem) reports only the
// slots in USE, which is capped at MAX_PROCESS_JOBS (1 by default), so it says
// nothing at all about how many jobs are stacked up behind them. Nothing in the
// process knew that number before these counters; the FE had no way to tell an
// uploader "there are 20 gifs ahead of yours".
var (
	// Waiting plus active. Incremented when a job is committed to, decremented
	// when it finishes — success, failure or early return alike.
	queueTotal atomic.Int64
	// Holding a processSem slot right now.
	queueActive atomic.Int64
)

// The only keys queueByKind will ever hold.
//
// Fixed rather than a map keyed by whatever arrives: `contents.filetype` is a
// client-supplied select field, and contents.createRule only requires canUpload,
// so an unrecognised value must not be able to grow this map without bound.
// Anything unknown is counted under "other".
var queueKinds = []string{"gif", "video", "image", "sticker", "convert", "other"}

var queueByKind = func() map[string]*atomic.Int64 {
	m := make(map[string]*atomic.Int64, len(queueKinds))
	for _, k := range queueKinds {
		m[k] = new(atomic.Int64)
	}
	return m
}()

// queueKind maps a filetype onto a counter bucket. Encode cost varies by an
// order of magnitude between them — a queue of 20 stickers clears in seconds
// where 20 videos is most of an hour — so a bare total would be a number the
// uploader can't act on.
func queueKind(filetype string) string {
	if _, ok := queueByKind[filetype]; ok {
		return filetype
	}
	return "other"
}

// enqueueJob records one job as being in the queue and returns its release.
//
// Call it SYNCHRONOUSLY at the moment the work is committed to — for uploads
// that is the record-create hook, not the goroutine it spawns. The goroutine may
// not be scheduled for a while and sleeps a second before doing anything, and a
// client polling in that window would otherwise see a queue shorter than it is.
//
// The release is idempotent: these counters have no floor, so one double
// decrement would corrupt the reported depth for the lifetime of the process.
func enqueueJob(filetype string) (release func()) {
	kind := queueKind(filetype)
	queueTotal.Add(1)
	queueByKind[kind].Add(1)

	var once sync.Once
	return func() {
		once.Do(func() {
			queueTotal.Add(-1)
			queueByKind[kind].Add(-1)
		})
	}
}

// beginActive marks an already-enqueued job as holding a process slot. Call it
// immediately after acquiring processSem and release it alongside the semaphore.
func beginActive() (release func()) {
	queueActive.Add(1)

	var once sync.Once
	return func() {
		once.Do(func() { queueActive.Add(-1) })
	}
}

// QueueSnapshot is the body of GET /api/queue.
type QueueSnapshot struct {
	// Waiting plus active — "how many items are in the queue".
	Total   int `json:"total"`
	Active  int `json:"active"`
	Waiting int `json:"waiting"`
	// How many jobs can encode at once (MAX_PROCESS_JOBS). Lets the client say
	// "one at a time" rather than implying the backlog drains in parallel.
	Capacity int            `json:"capacity"`
	ByKind   map[string]int `json:"by_kind"`
}

func queueSnapshot() QueueSnapshot {
	// Total FIRST. The two are independent atomics, so a job finishing between
	// the loads has to be able to make the snapshot stale, not inconsistent:
	// reading active first would let total drop underneath it and yield a
	// negative Waiting.
	total := int(queueTotal.Load())
	active := int(queueActive.Load())
	if active > total {
		active = total
	}

	byKind := make(map[string]int, len(queueKinds))
	for _, k := range queueKinds {
		if n := int(queueByKind[k].Load()); n > 0 {
			byKind[k] = n
		}
	}

	return QueueSnapshot{
		Total:    total,
		Active:   active,
		Waiting:  total - active,
		Capacity: cap(processSem),
		ByKind:   byKind,
	}
}

// RegisterQueueRoutes exposes the encode queue depth.
//
//	GET /api/queue -> 200 {"total":30,"active":1,"waiting":29,"capacity":1,"by_kind":{"gif":28,"video":2}}
//
// A custom route rather than something PocketBase-native, which needs the
// justification: the queue is not data. It is goroutines blocked on an in-memory
// channel, so no collection holds it and therefore no API rule or record hook
// can serve it.
//
// The PB-native option was deriving it from `contents` — count records that have
// a file but no preview yet — and it lies. Every encode failure path in
// moveFileToCustomR2Path returns before `preview` is ever set, so those records
// stay preview-less forever and the derived count only grows; it would also miss
// bot reuploads and /api/convert calls, which hold the same semaphore without
// creating a record. Mirroring these counters into a collection instead would
// mean a write plus a realtime broadcast per job transition to publish a number
// that already lives in RAM.
//
// Authenticated because the only consumer is the upload page, which is behind
// auth already. Deliberately NOT gated on canUpload — the number is harmless and
// not worth an uploader lookup per poll.
func RegisterQueueRoutes(app *pocketbase.PocketBase) {
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.GET("/api/queue", func(e *core.RequestEvent) error {
			return e.JSON(200, queueSnapshot())
		}).Bind(apis.RequireAuth("users"))
		return e.Next()
	})
}
