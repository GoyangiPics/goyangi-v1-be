package hooks

import (
	"testing"
	"time"
)

func TestRailwayDrainBudget(t *testing.T) {
	for _, tc := range []struct {
		env  string
		want time.Duration
	}{
		{"", 0},
		{"nope", 0},
		{"10", 0}, // shorter than the margin: not worth waiting
		{"600", 600*time.Second - railwayDrainMargin},
		{" 120 ", 120*time.Second - railwayDrainMargin},
	} {
		t.Setenv("RAILWAY_DEPLOYMENT_DRAINING_SECONDS", tc.env)
		if got := railwayDrainBudget(); got != tc.want {
			t.Errorf("RAILWAY_DEPLOYMENT_DRAINING_SECONDS=%q: budget %s, want %s", tc.env, got, tc.want)
		}
	}
}

func TestWaitIdleDeadline(t *testing.T) {
	// Idle right away: true without waiting for the deadline.
	if !waitIdle(0, time.Now().Add(time.Minute)) {
		t.Fatal("idle server: waitIdle = false, want true")
	}

	// Busy past the deadline: false.
	convertGate <- struct{}{}
	defer func() { <-convertGate }()
	start := time.Now()
	if waitIdle(0, time.Now().Add(50*time.Millisecond)) {
		t.Fatal("busy server: waitIdle = true, want false")
	}
	if waited := time.Since(start); waited > drainPoll+time.Second {
		t.Fatalf("waited %s past a 50ms deadline", waited)
	}
}
