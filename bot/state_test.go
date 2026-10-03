package bot

import (
	"testing"
	"time"
)

// resetChains clears the package-level registry between cases. These tests share
// process state, so each one starts from empty rather than relying on unique
// keys.
func resetChains(t *testing.T) {
	t.Helper()
	chainMu.Lock()
	chains = map[string]chainEntry{}
	chainMu.Unlock()
}

// ageChain backdates a chain's last activity, which is the only way to exercise
// the window without waiting five real minutes or injecting a clock.
func ageChain(t *testing.T, channelID, authorID string, by time.Duration) {
	t.Helper()
	chainMu.Lock()
	defer chainMu.Unlock()
	key := chainKey(channelID, authorID)
	entry, ok := chains[key]
	if !ok {
		t.Fatalf("no chain for %s", key)
	}
	entry.lastAt = entry.lastAt.Add(-by)
	chains[key] = entry
}

func TestChainLifecycle(t *testing.T) {
	const (
		channel = "chan1"
		author  = "author1"
	)

	t.Run("nothing open means no continuation", func(t *testing.T) {
		resetChains(t)
		if _, ok := peekChain(channel, author); ok {
			t.Error("peekChain succeeded with no chain open")
		}
	})

	t.Run("an open chain hands back its set metadata", func(t *testing.T) {
		resetChains(t)
		openChain(channel, author, Metadata{SetId: "set1", Idol: "Yujin"})
		meta, ok := peekChain(channel, author)
		if !ok {
			t.Fatal("peekChain failed on an open chain")
		}
		if meta.SetId != "set1" || meta.Idol != "Yujin" {
			t.Errorf("metadata = %+v", meta)
		}
	})

	t.Run("peek does not spend a slot", func(t *testing.T) {
		resetChains(t)
		openChain(channel, author, Metadata{SetId: "set1"})
		for range chainMaxFollowUps + 2 {
			if _, ok := peekChain(channel, author); !ok {
				t.Fatal("peeking alone exhausted the chain")
			}
		}
	})

	t.Run("the follow-up budget is finite", func(t *testing.T) {
		resetChains(t)
		openChain(channel, author, Metadata{SetId: "set1"})
		for i := range chainMaxFollowUps {
			if _, ok := peekChain(channel, author); !ok {
				t.Fatalf("follow-up %d was refused, want allowed", i+1)
			}
			consumeChain(channel, author)
		}
		if _, ok := peekChain(channel, author); ok {
			t.Errorf("follow-up %d was allowed, want refused", chainMaxFollowUps+1)
		}
	})

	t.Run("a new ping restarts the budget", func(t *testing.T) {
		resetChains(t)
		openChain(channel, author, Metadata{SetId: "set1"})
		for range chainMaxFollowUps {
			consumeChain(channel, author)
		}
		openChain(channel, author, Metadata{SetId: "set2"})
		meta, ok := peekChain(channel, author)
		if !ok {
			t.Fatal("a fresh chain was refused")
		}
		if meta.SetId != "set2" {
			t.Errorf("SetId = %q, want the newer set", meta.SetId)
		}
	})

	t.Run("the window closes the chain", func(t *testing.T) {
		resetChains(t)
		openChain(channel, author, Metadata{SetId: "set1"})
		ageChain(t, channel, author, chainWindow+time.Second)
		if _, ok := peekChain(channel, author); ok {
			t.Error("a chain past its window still accepted a follow-up")
		}
	})

	t.Run("the window slides on each follow-up", func(t *testing.T) {
		resetChains(t)
		openChain(channel, author, Metadata{SetId: "set1"})
		// Almost expired, then a follow-up lands — the next one is measured from
		// that, not from the ping, so a slow drop stays one set.
		ageChain(t, channel, author, chainWindow-time.Second)
		consumeChain(channel, author)
		ageChain(t, channel, author, chainWindow-time.Second)
		if _, ok := peekChain(channel, author); !ok {
			t.Error("the window was measured from the ping rather than the last message")
		}
	})
}

func TestChainsAreScopedPerChannelAndAuthor(t *testing.T) {
	resetChains(t)
	openChain("chan1", "author1", Metadata{SetId: "set1"})

	if _, ok := peekChain("chan1", "author2"); ok {
		t.Error("another author in the same channel joined the chain")
	}
	if _, ok := peekChain("chan2", "author1"); ok {
		t.Error("the same author in another channel joined the chain")
	}
	if _, ok := peekChain("chan1", "author1"); !ok {
		t.Error("the owning author lost their own chain")
	}
}

func TestConsumeUnknownChainIsSafe(t *testing.T) {
	resetChains(t)
	// Reachable if the chain expires between the peek and the consume.
	consumeChain("chan1", "author1")
	if _, ok := peekChain("chan1", "author1"); ok {
		t.Error("consuming a missing chain created one")
	}
}

func TestSameSubject(t *testing.T) {
	same := []struct{ a, b Metadata }{
		{Metadata{Idol: "Yujin", Group: "IVE"}, Metadata{Idol: "Yujin", Group: "IVE"}},
		// Role order is whatever Discord hands back, so it can't be significant.
		{Metadata{Idol: "Yujin, Gaeul", Group: "IVE"}, Metadata{Idol: "Gaeul, Yujin", Group: "IVE"}},
		{Metadata{Idol: "yujin", Group: "ive"}, Metadata{Idol: "Yujin", Group: "IVE"}},
		{Metadata{Idol: " Yujin ,Gaeul", Group: "IVE"}, Metadata{Idol: "Yujin, Gaeul", Group: "IVE"}},
	}
	for i, tt := range same {
		if !sameSubject(tt.a, tt.b) {
			t.Errorf("case %d: want same subject\n a: %+v\n b: %+v", i, tt.a, tt.b)
		}
	}

	different := []struct{ a, b Metadata }{
		{Metadata{Idol: "Yujin", Group: "IVE"}, Metadata{Idol: "Gaeul", Group: "IVE"}},
		{Metadata{Idol: "Yujin", Group: "IVE"}, Metadata{Idol: "Yujin", Group: "ITZY"}},
		// A superset is a different drop on purpose: merging two sets later is
		// easy, splitting a wrongly merged one is not.
		{Metadata{Idol: "Yujin", Group: "IVE"}, Metadata{Idol: "Yujin, Gaeul", Group: "IVE"}},
		// Nothing resolved on either side must never count as a match.
		{Metadata{}, Metadata{}},
	}
	for i, tt := range different {
		if sameSubject(tt.a, tt.b) {
			t.Errorf("case %d: want different subjects\n a: %+v\n b: %+v", i, tt.a, tt.b)
		}
	}
}
