package bot

import (
	"strings"
	"testing"
)

// TestParseSetLink covers the id shapes real set links carry. These ids come
// from hooks.generateContentId (date-group-idol-suffix), so they are full of
// hyphens — an earlier `^[a-zA-Z0-9]+$` guard here rejected every real link.
func TestParseSetLink(t *testing.T) {
	tests := []struct {
		link     string
		wantKind string
		wantID   string
	}{
		{"https://goyangi.pics/set/260727-ive-leeseo-b4a29354", "set", "260727-ive-leeseo-b4a29354"},
		{"https://goyangi.pics/set/yv5dzbdxz04lap5", "set", "yv5dzbdxz04lap5"},
		// mix- prefixed ids, for multi-group sets
		{"https://goyangi.pics/set/mix-260727-ive-aespa-unknown-b4a2", "set", "mix-260727-ive-aespa-unknown-b4a2"},
		{"https://goyangi.pics/collection/abc123", "collection", "abc123"},
		// Straight out of the browser: trailing slash, query, fragment.
		{"https://goyangi.pics/set/260727-ive-leeseo-b4a29354/", "set", "260727-ive-leeseo-b4a29354"},
		{"https://goyangi.pics/set/260727-ive-leeseo-b4a29354?page=2", "set", "260727-ive-leeseo-b4a29354"},
		{"https://goyangi.pics/set/260727-ive-leeseo-b4a29354#top", "set", "260727-ive-leeseo-b4a29354"},
		// A percent-escaped id decodes before it reaches the query.
		{"https://goyangi.pics/set/f%28x%29-260727-abcd", "set", "f(x)-260727-abcd"},
	}
	for _, tt := range tests {
		ref, err := parseSetLink(tt.link)
		if err != nil {
			t.Errorf("%s: unexpected error %v", tt.link, err)
			continue
		}
		if ref.kind != tt.wantKind || ref.id != tt.wantID {
			t.Errorf("%s:\n got kind=%q id=%q\nwant kind=%q id=%q",
				tt.link, ref.kind, ref.id, tt.wantKind, tt.wantID)
		}
		// The id must reach the query as a bound parameter, never inlined.
		if !strings.Contains(ref.filter, "{:id}") || ref.params()["id"] != tt.wantID {
			t.Errorf("%s: id must be bound, got filter=%q params=%v", tt.link, ref.filter, ref.params())
		}
	}

	// A pasted link is rebuilt into its canonical form.
	ref, err := parseSetLink("https://goyangi.pics/set/260727-ive-leeseo-b4a29354?page=2")
	if err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if want := publicBaseURL + "/set/260727-ive-leeseo-b4a29354"; ref.pageLink() != want {
		t.Errorf("pageLink() = %q, want %q", ref.pageLink(), want)
	}

	// Anything without a /set/<id> or /collection/<id> pair is rejected.
	for _, link := range []string{
		"https://goyangi.pics/single/abc123",
		"https://goyangi.pics/set", // no id follows
		"https://goyangi.pics/set/",
		"https://goyangi.pics/",
		"not a link at all",
		"",
	} {
		if _, err := parseSetLink(link); err == nil {
			t.Errorf("%q should not parse as a set link", link)
		}
	}
}

// TestPathSegmentAfter pins the segment extraction the link parsers share.
func TestPathSegmentAfter(t *testing.T) {
	if got, ok := pathSegmentAfter("https://goyangi.pics/single/260727-ive-leeseo-b4a2", "single"); !ok || got != "260727-ive-leeseo-b4a2" {
		t.Errorf("content page id: got %q (ok=%v)", got, ok)
	}
	// Whitespace from a copy-paste is tolerated.
	if got, ok := pathSegmentAfter("  https://goyangi.pics/single/abc  ", "single"); !ok || got != "abc" {
		t.Errorf("padded link: got %q (ok=%v)", got, ok)
	}
	// The segment name appearing as the id itself must not match.
	if _, ok := pathSegmentAfter("https://goyangi.pics/single", "single"); ok {
		t.Error("a trailing segment with nothing after it must not match")
	}
	// A bare id with no host still parses — url.Parse treats it as a path.
	if got, ok := pathSegmentAfter("/single/abc", "single"); !ok || got != "abc" {
		t.Errorf("host-less path: got %q (ok=%v)", got, ok)
	}
	if _, ok := pathSegmentAfter("https://i.imgur.com/abc123.mp4", "single"); ok {
		t.Error("an imgur link has no /single/ segment")
	}
}

// TestNormalizeImgurOption pins the page-link → direct-mp4 rewrite that lets a
// pasted imgur.com link match a stored mirror.
func TestNormalizeImgurOption(t *testing.T) {
	if got, want := normalizeImgurOption("https://imgur.com/abc123"), "https://i.imgur.com/abc123.mp4"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Already-direct links and non-imgur links pass through untouched.
	for _, link := range []string{
		"https://i.imgur.com/abc123.mp4",
		"https://cdn.goyangi.pics/v1/ITZY/Yuna/clip.mp4",
		"https://goyangi.pics/single/abc123",
	} {
		if got := normalizeImgurOption(link); got != link {
			t.Errorf("%q should pass through, got %q", link, got)
		}
	}
}
