package bot

import "sync/atomic"

// handlersInFlight counts Discord event handlers currently running. An ingest
// downloads media before it creates any record, so for that stretch nothing in
// hooks' encode queue knows the work exists.
var handlersInFlight atomic.Int64

// flushesInFlight counts batch flushes (AVIF notices, upload announcements)
// currently posting to Discord.
var flushesInFlight atomic.Int64

// tracked wraps an event listener so it is counted in handlersInFlight.
func tracked[E any](f func(E)) func(E) {
	return func(e E) {
		handlersInFlight.Add(1)
		defer handlersInFlight.Add(-1)
		f(e)
	}
}

// Busy reports how much bot work would be lost if the process exited now:
// running event handlers, batched Discord posts still waiting out their
// debounce window, and flushes in progress. Zero means safe to stop. Used by
// the supervised-restart drain (hooks/drain.go), wired in main.go.
func Busy() int {
	n := int(handlersInFlight.Load() + flushesInFlight.Load())

	batchMu.Lock()
	n += len(batch)
	batchMu.Unlock()

	uploadPostMu.Lock()
	n += len(pendingUploads)
	uploadPostMu.Unlock()

	return n
}
