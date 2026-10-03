package hooks

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestR2KeyFromURL(t *testing.T) {
	const base = "https://cdn.goyangi.pics"
	t.Setenv("R2_PUBLIC_URL", base)

	tests := []struct {
		name    string
		url     string
		wantKey string
		wantOK  bool
	}{
		{"plain object", base + "/v1/ive/leeseo/260727-ive-leeseo-a1b2.mp4", "v1/ive/leeseo/260727-ive-leeseo-a1b2.mp4", true},
		{"preview rendition", base + "/v1/mix/260727-x-y-a1b2-preview.avif", "v1/mix/260727-x-y-a1b2-preview.avif", true},
		{"empty", "", "", false},
		// The load-bearing cases: `source`/`mirror`/`discord` hold third-party
		// URLs, and turning one of those into a delete would be a request to a
		// bucket we don't own at best.
		{"third-party host", "https://i.imgur.com/abc.mp4", "", false},
		{"bare path", "/v1/ive/x.mp4", "", false},
		{"base itself", base, "", false},
		{"base with slash", base + "/", "", false},
		// Prefix-but-not-path: TrimPrefix on base+"/" must not match this.
		{"lookalike host", "https://cdn.goyangi.pics.evil.test/v1/x.mp4", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, ok := r2KeyFromURL(tt.url)
			if ok != tt.wantOK || key != tt.wantKey {
				t.Fatalf("r2KeyFromURL(%q) = (%q, %v), want (%q, %v)",
					tt.url, key, ok, tt.wantKey, tt.wantOK)
			}
		})
	}
}

func TestR2KeyFromURLWithoutBase(t *testing.T) {
	// RegisterR2Hooks refuses to boot without R2_PUBLIC_URL, but the helper must
	// still fail closed rather than deriving a key from a bare URL.
	t.Setenv("R2_PUBLIC_URL", "")
	if key, ok := r2KeyFromURL("https://cdn.goyangi.pics/v1/x.mp4"); ok {
		t.Fatalf("expected no key without R2_PUBLIC_URL, got %q", key)
	}
}

func TestR2KeyFromURLTrailingSlashBase(t *testing.T) {
	t.Setenv("R2_PUBLIC_URL", "https://cdn.goyangi.pics/")
	key, ok := r2KeyFromURL("https://cdn.goyangi.pics/v1/x.mp4")
	if !ok || key != "v1/x.mp4" {
		t.Fatalf("got (%q, %v), want (%q, true)", key, ok, "v1/x.mp4")
	}
}

func TestStaleR2URLs(t *testing.T) {
	tests := []struct {
		name          string
		before, after []string
		want          []string
	}{
		{
			name:   "nothing changed",
			before: []string{"a.mp4", "b.avif"},
			after:  []string{"a.mp4", "b.avif"},
			want:   nil,
		},
		{
			// The reprocess case: preview_format flipped avif -> webp.
			name:   "preview format swapped",
			before: []string{"x.mp4", "x.avif", "x-static.avif"},
			after:  []string{"x.mp4", "x.webp", "x-static.webp"},
			want:   []string{"x.avif", "x-static.avif"},
		},
		{
			name:   "first pass has no previous objects",
			before: nil,
			after:  []string{"x.mp4"},
			want:   nil,
		},
		{
			name:   "renditions added, none dropped",
			before: []string{"x.mp4"},
			after:  []string{"x.mp4", "x-sd.mp4", "x-static.avif"},
			want:   nil,
		},
		{
			name:   "everything dropped",
			before: []string{"x.mp4", "x.avif"},
			after:  nil,
			want:   []string{"x.mp4", "x.avif"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := staleR2URLs(tt.before, tt.after)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("staleR2URLs(%v, %v) = %v, want %v", tt.before, tt.after, got, tt.want)
			}
		})
	}
}

func TestEnqueueR2DeleteKeysSkipsEmptyAndSurvivesFullQueue(t *testing.T) {
	// Drain anything a previous test left behind.
	for len(r2DeleteQueue) > 0 {
		<-r2DeleteQueue
	}

	enqueueR2DeleteKeys("", "a", "", "b")
	if got := len(r2DeleteQueue); got != 2 {
		t.Fatalf("queued %d keys, want 2 (empties must be dropped)", got)
	}
	for len(r2DeleteQueue) > 0 {
		<-r2DeleteQueue
	}

	// A full queue must not block a delete hook — it logs and moves on.
	full := make([]string, r2DeleteQueueSize+16)
	for i := range full {
		full[i] = "k"
	}
	done := make(chan struct{})
	go func() {
		enqueueR2DeleteKeys(full...)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("enqueueR2DeleteKeys blocked on a full queue")
	}

	for len(r2DeleteQueue) > 0 {
		<-r2DeleteQueue
	}
}

func TestStillScaleCapCapsLongEdgeWhateverTheOrientation(t *testing.T) {
	// Guards the actual bug: scaleDown1080p took min() of the SOURCE on both
	// axes against an asymmetric 1920x1080 box, so portrait content was boxed at
	// 1080x1080 and crushed. A square box is what makes the cap orientation-
	// agnostic, so assert the box is square and the required flags are present.
	got := stillScaleCap(2560)
	for _, want := range []string{
		"min(iw,2560)",
		"min(ih,2560)",
		"force_original_aspect_ratio=decrease",
		"force_divisible_by=2",
		"out_range=pc",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stillScaleCap(2560) = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "1080") || strings.Contains(got, "1920") {
		t.Errorf("stillScaleCap(2560) = %q, still carries the old 1080p box", got)
	}
}
