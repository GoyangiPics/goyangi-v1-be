package bot

import (
	"log/slog"
	"sync"
	"time"

	"github.com/disgoorg/snowflake/v2"
)

// Shared mutable state touched by concurrently-running handlers lives here,
// always behind a mutex: slash commands, the ingest API's requests and the
// gateway's message handler all run at once, so unsynchronized package globals
// would be data races (and a racing map write is a fatal runtime crash).

const stateTTL = time.Hour

// ─── Set registry ───────────────────────────────────────────────────────────
// When a multi-item message creates a "set", we remember it keyed by the
// origin message ID so a follow-up reply to that message can attach to the
// same set. Replaces the old single `lastMetadata` global (which both raced
// and could only remember the most recent set).

type setEntry struct {
	meta      Metadata
	createdAt time.Time
}

var (
	setRegistryMu sync.Mutex
	setRegistry   = map[string]setEntry{}
)

func rememberSet(messageID string, meta Metadata) {
	setRegistryMu.Lock()
	defer setRegistryMu.Unlock()
	for id, e := range setRegistry {
		if time.Since(e.createdAt) > stateTTL {
			delete(setRegistry, id)
		}
	}
	setRegistry[messageID] = setEntry{meta: meta, createdAt: time.Now()}
}

func lookupSet(messageID string) (Metadata, bool) {
	setRegistryMu.Lock()
	defer setRegistryMu.Unlock()
	e, ok := setRegistry[messageID]
	// Swept only when a new set is remembered, so an entry can outlive the
	// hour; it must not still answer once it has.
	if !ok || time.Since(e.createdAt) > stateTTL {
		return Metadata{}, false
	}
	return e.meta, true
}

// forgetSet drops a set that ended up holding nothing (runIngestion deletes
// it), so a reply can't be routed into a set that no longer exists.
func forgetSet(messageID string) {
	setRegistryMu.Lock()
	defer setRegistryMu.Unlock()
	delete(setRegistry, messageID)
}

// ─── Open ingestion chains ──────────────────────────────────────────────────
// Plenty of people post a set as a run of separate messages rather than one
// message or a reply chain: the first pings a role, the rest are bare
// follow-ups. Those follow-ups carry no trigger of their own, so before this
// they were dropped outright — the first message became a one-item set and the
// rest of the drop was simply lost.
//
// So after a ping creates a set, the channel+poster pair holds an "open chain"
// briefly, and the next few media-only messages from that poster join it.
//
// The window runs on Discord's clock — when each message was POSTED — not on
// ours. Live, the two agree; but a backlog forwarded after downtime is
// processed within seconds, and on our clock two drops posted an hour apart
// would read as one.
//
// This is a heuristic and it will occasionally be wrong — somebody posting an
// unrelated clip right after their own pinged drop gets it folded in. That is
// the deliberate trade: a mod can split a set afterwards, but nothing can
// recover content that was never collected.

const (
	// chainWindow is how long a chain stays open, measured from the last
	// message that joined it rather than from the ping — a slow uploader
	// posting five files over four minutes is one drop, not five.
	chainWindow = 5 * time.Minute

	// chainMaxFollowUps caps how many bare messages one ping can absorb.
	chainMaxFollowUps = 3
)

type chainEntry struct {
	meta Metadata
	// lastAt is when the last message that joined was posted (Discord's clock).
	lastAt time.Time
	// touched is our clock, and only decides when the entry is evicted.
	touched  time.Time
	attached int
}

var (
	chainMu sync.Mutex
	chains  = map[string]chainEntry{}
)

// Keyed per channel as well as per poster: the same person dropping in two
// channels at once is two independent chains. The poster key is whoever the
// post is credited to (see poster in ingest.go) — for a relay app's posts,
// that is the person behind it, not the app.
func chainKey(channelID, posterKey string) string { return channelID + ":" + posterKey }

// openChain starts (or restarts) the chain for a poster in a channel, as of
// `at`, the opening message's posting time. A fresh ping resets the follow-up
// count, so each drop gets its own budget.
func openChain(channelID, posterKey string, meta Metadata, at time.Time) {
	chainMu.Lock()
	defer chainMu.Unlock()
	for key, entry := range chains {
		if time.Since(entry.touched) > stateTTL {
			delete(chains, key)
		}
	}
	chains[chainKey(channelID, posterKey)] = chainEntry{meta: meta, lastAt: at, touched: time.Now()}
}

// peekChain reports the set a bare follow-up posted at `at` would join, without
// spending a slot. Consuming is a separate step so a message that turns out to
// be a redelivery doesn't eat into the budget.
func peekChain(channelID, posterKey string, at time.Time) (Metadata, bool) {
	chainMu.Lock()
	defer chainMu.Unlock()
	entry, ok := chains[chainKey(channelID, posterKey)]
	if !ok || entry.attached >= chainMaxFollowUps {
		return Metadata{}, false
	}
	gap := at.Sub(entry.lastAt)
	if gap < 0 {
		gap = -gap
	}
	if gap > chainWindow {
		return Metadata{}, false
	}
	return entry.meta, true
}

// consumeChain records that a follow-up posted at `at` joined, and slides the
// window forward.
func consumeChain(channelID, posterKey string, at time.Time) {
	chainMu.Lock()
	defer chainMu.Unlock()
	key := chainKey(channelID, posterKey)
	entry, ok := chains[key]
	if !ok {
		return
	}
	entry.attached++
	if at.After(entry.lastAt) {
		entry.lastAt = at
	}
	entry.touched = time.Now()
	chains[key] = entry
}

// ─── Processed-message dedup ─────────────────────────────────────────────────
// Discord can redeliver the same MessageCreate; claimMessage returns false if
// we've already handled this message ID, preventing duplicate content records.

var (
	processedMu   sync.Mutex
	processedMsgs = map[string]time.Time{}
)

func claimMessage(messageID string) bool {
	processedMu.Lock()
	defer processedMu.Unlock()
	now := time.Now()
	for id, t := range processedMsgs {
		if now.Sub(t) > stateTTL {
			delete(processedMsgs, id)
		}
	}
	if _, seen := processedMsgs[messageID]; seen {
		return false
	}
	processedMsgs[messageID] = now
	return true
}

// isClaimed reports whether a message is claimed, without claiming it.
func isClaimed(messageID string) bool {
	processedMu.Lock()
	defer processedMu.Unlock()
	t, seen := processedMsgs[messageID]
	return seen && time.Since(t) <= stateTTL
}

// releaseClaim gives a message back, after a run that saved nothing: a retry
// should get a real second attempt, not "already handled".
func releaseClaim(messageID string) {
	processedMu.Lock()
	defer processedMu.Unlock()
	delete(processedMsgs, messageID)
}

// ─── Per-channel order ───────────────────────────────────────────────────────
// Follow-ups, replies and repeat pings read what the message before them left
// behind — the set registry and the open chains above — so a channel's
// messages have to be handled one at a time, in the order they arrived.
//
// Our own gateway always did this without trying: disgo runs listeners inline
// in its read loop (the bot doesn't enable async events), so message N+1 isn't
// looked at until message N has finished. The ingest API takes requests
// concurrently, though, and both doors can be open at once. A channel's turn is
// that ordering, made explicit and shared by both: first come, first served,
// one message at a time per channel. Different channels don't wait for each
// other.

// channelTurn is one message's place in its channel's line.
type channelTurn struct {
	channel snowflake.ID
	prev    chan struct{} // closed when the message ahead is done; nil if none
	done    chan struct{}
}

var (
	turnsMu   sync.Mutex
	turnTails = map[snowflake.ID]chan struct{}{}
)

// queueTurn takes the next place in a channel's line. It doesn't block, so a
// caller can hold its place from the moment a message arrives and wait for its
// turn somewhere else (the ingest API queues at request time, then waits in the
// job goroutine).
func queueTurn(channel snowflake.ID) *channelTurn {
	turnsMu.Lock()
	defer turnsMu.Unlock()
	t := &channelTurn{channel: channel, prev: turnTails[channel], done: make(chan struct{})}
	turnTails[channel] = t.done
	return t
}

// wait blocks until every message queued ahead in the channel is done.
func (t *channelTurn) wait() {
	if t.prev != nil {
		<-t.prev
	}
}

// release lets the next message in the channel go. Must run exactly once.
func (t *channelTurn) release() {
	turnsMu.Lock()
	if turnTails[t.channel] == t.done {
		delete(turnTails, t.channel)
	}
	turnsMu.Unlock()
	close(t.done)
}

// createByNameMu serializes lookup-or-create of named records (uploaders,
// tags) so two concurrent messages introducing the same new name can't both
// create it (TOCTOU).
var createByNameMu sync.Mutex

// ─── Handler panic guard ─────────────────────────────────────────────────────
// A panic in an event handler goroutine would otherwise crash the whole
// process (the bot shares the PocketBase process).

func recoverHandler(name string) {
	if r := recover(); r != nil {
		slog.Error("recovered from panic in handler", "handler", name, "panic", r)
	}
}
