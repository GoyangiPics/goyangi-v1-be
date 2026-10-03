package bot

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBlockNonPublic(t *testing.T) {
	blocked := []string{
		"127.0.0.1:80",       // loopback
		"10.0.0.1:80",        // RFC1918
		"172.16.0.1:8080",    // RFC1918
		"192.168.1.5:443",    // RFC1918
		"169.254.169.254:80", // link-local — cloud metadata lives here
		"0.0.0.0:80",         // unspecified
		"224.0.0.1:80",       // multicast
		"[::1]:80",           // v6 loopback
		"[fe80::1]:80",       // v6 link-local
		"[fc00::1]:80",       // v6 ULA
		"[::]:80",            // v6 unspecified
	}
	for _, addr := range blocked {
		if err := blockNonPublic("tcp", addr, nil); err == nil {
			t.Errorf("blockNonPublic(%q) = nil, want refusal", addr)
		}
	}

	allowed := []string{
		"1.1.1.1:443",
		"93.184.216.34:80",
		"[2606:4700:4700::1111]:443",
	}
	for _, addr := range allowed {
		if err := blockNonPublic("tcp", addr, nil); err != nil {
			t.Errorf("blockNonPublic(%q) = %v, want nil", addr, err)
		}
	}

	// The dialer hands over host:port; anything else is refused, not guessed at.
	if err := blockNonPublic("tcp", "no-port-here", nil); err == nil {
		t.Error("blockNonPublic without a port should refuse")
	}
	if err := blockNonPublic("tcp", "unresolved.example.com:80", nil); err == nil {
		t.Error("blockNonPublic with a non-IP host should refuse")
	}
}

// The wiring test: a real client with the transport must refuse to fetch from
// a loopback server — the exact SSRF this exists to stop.
func TestMediaDownloadClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("should never be reachable"))
	}))
	defer srv.Close()

	_, _, _, err := downloadFile(srv.URL + "/x.mp4")
	if err == nil {
		t.Fatal("downloadFile fetched from loopback; the dial guard is not wired")
	}
	if !errors.Is(err, errPrivateAddress) && !strings.Contains(err.Error(), errPrivateAddress.Error()) {
		t.Fatalf("downloadFile failed for the wrong reason: %v", err)
	}
}
