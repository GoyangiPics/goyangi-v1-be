package main

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// Fetching the media itself.
//
// The one rule that matters here is imgur's. A link that 404s in a browser is
// often still downloadable — imgur's web page and its CDN disagree about what
// has been deleted — so retrievability is decided by attempting the actual
// download, never by a page check. And a link that IS gone doesn't fail: imgur
// answers 200 with a redirect to its "removed.png" placeholder, which would
// otherwise be archived as a 161×81 still with a straight face. Both of those
// are what unretrievable means below; nothing else is inferred.

// errUnretrievable marks a link whose content is gone. The item is dropped
// (requirement: omit hard-deleted imgur links), as opposed to a transient error,
// which fails the item and is retried on the next run.
var errUnretrievable = errors.New("unretrievable")

const maxDownloadBytes = 256 << 20 // the server's per-file ceiling

// browserUA: imgur refuses requests without one (see bot.downloadFile).
const browserUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/119.0.0.0 Safari/537.36"

// Five minutes for a download, but only thirty seconds to start answering: the
// old-history channels are full of links to hosts that are gone (gfycat) or
// that accept the connection and never respond (fileditchstuff.me), and a
// slot spent five minutes waiting for headers on each of those was costing
// more than the encodes.
var mediaClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		Proxy:                 http.ProxyFromEnvironment,
	},
}

type fetched struct {
	data        []byte
	contentType string
	filename    string // from Content-Disposition, "" when absent
	finalURL    string
}

// download fetches one media URL.
func download(link string) (*fetched, error) {
	return fetch(link, "")
}

// probe is download's cheap cousin for the dry run: the same request with a
// Range header, so a live link costs a kilobyte and a dead one is still told
// apart the same way. Some hosts ignore Range and send the whole body; that is
// tolerated, only slower.
func probe(link string) error {
	_, err := fetch(link, "bytes=0-1023")
	return err
}

func fetch(link, byteRange string) (*fetched, error) {
	req, err := http.NewRequest(http.MethodGet, link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", browserUA)
	if byteRange != "" {
		req.Header.Set("Range", byteRange)
	}
	resp, err := mediaClient.Do(req)
	if err != nil {
		// A host that no longer resolves is gone for good — gfycat, mostly.
		// Everything else (timeouts, resets) stays transient: retried on a
		// rerun, never mistaken for a deletion.
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			return nil, fmt.Errorf("%w: host no longer exists", errUnretrievable)
		}
		return nil, err
	}
	defer resp.Body.Close()

	final := resp.Request.URL.String()
	if strings.Contains(strings.ToLower(resp.Request.URL.Path), "removed.png") {
		return nil, fmt.Errorf("%w: imgur removed placeholder", errUnretrievable)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, fmt.Errorf("%w: HTTP %d", errUnretrievable, resp.StatusCode)
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
		// 403/429/5xx: could be rate limiting or a bad day. Not a verdict on the
		// content, so not unretrievable.
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	ct := strings.ToLower(strings.TrimSpace(strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]))
	if strings.HasPrefix(ct, "text/") || ct == "application/json" || ct == "application/xhtml+xml" {
		// A media URL answering with a page is a "this is gone" page.
		return nil, fmt.Errorf("%w: got %s instead of media", errUnretrievable, ct)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownloadBytes {
		return nil, fmt.Errorf("exceeds the %d MB limit", maxDownloadBytes>>20)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty body", errUnretrievable)
	}

	out := &fetched{data: data, contentType: resp.Header.Get("Content-Type"), finalURL: final}
	if _, params, perr := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); perr == nil {
		if name := path.Base(params["filename"]); name != "." && name != "/" {
			out.filename = name
		}
	}
	return out, nil
}
