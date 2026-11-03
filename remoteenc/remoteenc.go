// Package remoteenc runs the backend's ffmpeg encodes on a separate worker
// over HTTP, so the server that hosts PocketBase doesn't need the CPU for them.
//
// EXPERIMENT (exp branch). The point is to measure speed, cost and viability
// of a pay-per-use encode worker (Railway first) before committing to the
// architecture. The protocol mirrors runFFmpegWith one-to-one — same args,
// one input, one output — so a remote encode is byte-for-byte the job a local
// one would have run, and the timings compare like for like.
//
// That also makes it a "run ffmpeg with these args" endpoint, guarded only by
// the bearer token. Fine for an experiment; a production worker should expose
// named operations instead, so the backend can't ask it for arbitrary work.
//
// Wire format: POST /encode, multipart/form-data with
//   - "spec":  JSON Spec
//   - "input": the input file, omitted when Spec.SourceKey names an R2 object
//     the worker fetches itself (the upload source, so it never crosses the
//     backend's own connection)
//
// 200 returns the output file with the metric headers below; any other status
// returns a plain-text error.
package remoteenc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RequiredEncoders are the ffmpeg encoders the upload pipeline cannot work
// without: AV1 for the canonical MP4 and AVIF previews, x264 for the SD
// fallback's CPU path, WebP for WebP previews, AAC for video audio. The
// backend's preflight and the worker's boot check both use this list.
var RequiredEncoders = []string{"libsvtav1", "libx264", "libwebp", "aac"}

// Metric headers on a successful /encode response.
const (
	HeaderWorkerMs     = "X-Encode-Worker-Ms"     // ffmpeg wall time on the worker
	HeaderFetchMs      = "X-Encode-Fetch-Ms"      // R2 fetch of the source; 0 when cached or sent in the body
	HeaderCPUMs        = "X-Encode-Cpu-Ms"        // ffmpeg user+system CPU time
	HeaderMaxRSSKB     = "X-Encode-Maxrss-Kb"     // ffmpeg peak resident memory
	HeaderServed       = "X-Encode-Served"        // requests this worker process has served, this one included
	HeaderSourceCached = "X-Encode-Source-Cached" // "1" when the source came from the worker's cache
)

// HeaderWorker is set on every response the worker itself writes, so the client
// can tell the worker's own 502/504 (a failed R2 fetch, an ffmpeg timeout) from
// the platform's while the worker is still booting.
const HeaderWorker = "X-Encode-Worker"

// Spec is one ffmpeg run: `ffmpeg <PreInput> -i <input> <Args> -y <output>`.
type Spec struct {
	PreInput  []string `json:"preInput,omitempty"`
	Args      []string `json:"args"`
	InExt     string   `json:"inExt"`
	OutExt    string   `json:"outExt"`
	SourceKey string   `json:"sourceKey,omitempty"`
}

var extRe = regexp.MustCompile(`^\.[a-z0-9]{1,5}$`)

// Validate rejects specs the worker won't run. Extensions end up in temp file
// names, so they are held to a short, path-free shape; a second -i would let a
// spec read files on the worker, so the input is always the one we place.
func (s Spec) Validate() error {
	if !extRe.MatchString(s.InExt) {
		return fmt.Errorf("bad input extension %q", s.InExt)
	}
	if !extRe.MatchString(s.OutExt) {
		return fmt.Errorf("bad output extension %q", s.OutExt)
	}
	if slices.Contains(s.PreInput, "-i") || slices.Contains(s.Args, "-i") {
		return fmt.Errorf("spec may not add inputs")
	}
	return nil
}

// Result is a finished remote encode plus what it cost.
type Result struct {
	Output       []byte
	TotalMs      int64 // round trip as the client saw it: upload, queueing, encode, download
	WorkerMs     int64
	FetchMs      int64
	CPUMs        int64
	MaxRSSKB     int64
	Served       int64
	SourceCached bool
}

// Client calls a worker.
type Client struct {
	URL   string // base URL, no trailing slash
	Token string
	HTTP  *http.Client
	// WakeTimeout bounds how long a call keeps retrying a worker that is
	// still booting. Default 90s.
	WakeTimeout time.Duration
}

const defaultWakeTimeout = 90 * time.Second

// wakeBackoff is the first wait between wake-up retries; it doubles up to 5s.
var wakeBackoff = 500 * time.Millisecond

// do sends the request newReq builds, retrying while a sleeping worker wakes
// (Railway serverless): until it is up, the connection is refused or the
// platform answers 502/503/504 itself. newReq runs once per attempt, since a
// streamed body can be read only once. Anything the worker answered is
// returned as is — retrying its own errors would rerun a failed encode.
func (c *Client) do(ctx context.Context, newReq func() (*http.Request, error)) (*http.Response, error) {
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	wake := c.WakeTimeout
	if wake <= 0 {
		wake = defaultWakeTimeout
	}
	deadline := time.Now().Add(wake)
	backoff := wakeBackoff
	for {
		req, err := newReq()
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(req)
		if !waking(resp, err) || time.Now().Add(backoff).After(deadline) {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		if req.Body != nil {
			req.Body.Close() // ends a streaming body's writer
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// waking reports whether a call failed because the worker isn't up yet.
func waking(resp *http.Response, err error) bool {
	if err != nil {
		var op *net.OpError
		var dns *net.DNSError
		return (errors.As(err, &op) && op.Op == "dial") || errors.As(err, &dns)
	}
	switch resp.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return resp.Header.Get(HeaderWorker) == ""
	}
	return false
}

// Health calls GET /healthz and returns the worker's self-report (ffmpeg
// version, CPUs, requests served) as JSON text.
func (c *Client) Health(ctx context.Context) (string, error) {
	resp, err := c.do(ctx, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/healthz", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		return req, nil
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s", resp.Status)
	}
	return strings.TrimSpace(string(body)), nil
}

// Encode runs spec on the worker. input is sent only when spec.SourceKey is
// empty.
func (c *Client) Encode(ctx context.Context, spec Spec, input []byte) (*Result, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return nil, err
	}

	// Streamed through a pipe: inputs run to ~100 MB, and buffering the whole
	// multipart body would hold a second copy in memory.
	newReq := func() (*http.Request, error) {
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		go func() {
			err := mw.WriteField("spec", string(specJSON))
			if err == nil && spec.SourceKey == "" {
				var part io.Writer
				if part, err = mw.CreateFormFile("input", "input"+spec.InExt); err == nil {
					_, err = part.Write(input)
				}
			}
			if err == nil {
				err = mw.Close()
			}
			pw.CloseWithError(err)
		}()

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/encode", pr)
		if err != nil {
			pr.Close()
			return nil, err
		}
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set("Authorization", "Bearer "+c.Token)
		return req, nil
	}

	start := time.Now()
	resp, err := c.do(ctx, newReq)
	if err != nil {
		return nil, fmt.Errorf("encode worker: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("encode worker: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("encode worker: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	h := resp.Header
	num := func(name string) int64 {
		n, _ := strconv.ParseInt(h.Get(name), 10, 64)
		return n
	}
	return &Result{
		Output:       body,
		TotalMs:      time.Since(start).Milliseconds(),
		WorkerMs:     num(HeaderWorkerMs),
		FetchMs:      num(HeaderFetchMs),
		CPUMs:        num(HeaderCPUMs),
		MaxRSSKB:     num(HeaderMaxRSSKB),
		Served:       num(HeaderServed),
		SourceCached: h.Get(HeaderSourceCached) == "1",
	}, nil
}
