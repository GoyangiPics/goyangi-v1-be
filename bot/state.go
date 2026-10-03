package bot

import (
	"log/slog"
	"sync"
	"time"
)

// Shared mutable state touched by concurrently-running event handlers lives
// here, always behind a mutex. disgo dispatches gateway events concurrently,
// so unsynchronized package globals are data races (and a racing map write is
// a fatal runtime crash).

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
	return e.meta, ok
}

// ─── Open ingestion chains ──────────────────────────────────────────────────
// Plenty of people post a set as a run of separate messages rather than one
// message or a reply chain: the first pings a role, the rest are bare
// follow-ups. Those follow-ups carry no trigger of their own, so before this
// they were dropped outright — the first message became a one-item set and the
// rest of the drop was simply lost.
//
// So after a ping creates a set, the channel+author pair holds an "open chain"
// briefly, and the next few media-only messages from that author join it.
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
	meta     Metadata
	lastAt   time.Time
	attached int
}

var (
	chainMu sync.Mutex
	chains  = map[string]chainEntry{}
)

// Keyed per channel as well as per author: the same person dropping in two
// channels at once is two independent chains.
func chainKey(channelID, authorID string) string { return channelID + ":" + authorID }

// openChain starts (or restarts) the chain for an author in a channel. A fresh
// ping resets the follow-up count, so each drop gets its own budget.
func openChain(channelID, authorID string, meta Metadata) {
	chainMu.Lock()
	defer chainMu.Unlock()
	for key, entry := range chains {
		if time.Since(entry.lastAt) > stateTTL {
			delete(chains, key)
		}
	}
	chains[chainKey(channelID, authorID)] = chainEntry{meta: meta, lastAt: time.Now()}
}

// peekChain reports the set a bare follow-up would join, without spending a
// slot. Consuming is a separate step so a message that turns out to be a
// gateway redelivery doesn't eat into the budget.
func peekChain(channelID, authorID string) (Metadata, bool) {
	chainMu.Lock()
	defer chainMu.Unlock()
	entry, ok := chains[chainKey(channelID, authorID)]
	if !ok || entry.attached >= chainMaxFollowUps || time.Since(entry.lastAt) > chainWindow {
		return Metadata{}, false
	}
	return entry.meta, true
}

// consumeChain records that a follow-up joined, and slides the window forward.
func consumeChain(channelID, authorID string) {
	chainMu.Lock()
	defer chainMu.Unlock()
	key := chainKey(channelID, authorID)
	entry, ok := chains[key]
	if !ok {
		return
	}
	entry.attached++
	entry.lastAt = time.Now()
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
