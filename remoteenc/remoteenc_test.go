//go:build unix

package remoteenc

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFFmpeg copies its -i input to its -y output, or fails when an arg is
// FAIL — enough to exercise the protocol without a real ffmpeg.
const fakeFFmpeg = `#!/bin/sh
in=""; out=""
while [ $# -gt 0 ]; do
  case "$1" in
    -i) in="$2"; shift 2;;
    -y) out="$2"; shift 2;;
    FAIL) echo "boom: bad input" >&2; exit 1;;
    *) shift;;
  esac
done
cp "$in" "$out"
`

type fakeSources struct {
	data  map[string][]byte
	calls atomic.Int32
}

func (f *fakeSources) Fetch(_ context.Context, key string, w io.Writer) error {
	f.calls.Add(1)
	b, ok := f.data[key]
	if !ok {
		return os.ErrNotExist
	}
	_, err := w.Write(b)
	return err
}

func newTestWorker(t *testing.T, src SourceFetcher) (*Client, func()) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(bin, []byte(fakeFFmpeg), 0o755); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Token: "secret", FFmpeg: bin, Sources: src, WorkDir: dir}
	ts := httptest.NewServer(srv.Handler())
	return &Client{URL: ts.URL, Token: "secret", HTTP: ts.Client()}, ts.Close
}

func TestEncodeBodyInput(t *testing.T) {
	c, done := newTestWorker(t, nil)
	defer done()

	in := []byte("pretend this is an mp4")
	res, err := c.Encode(context.Background(), Spec{Args: []string{"-an"}, InExt: ".mp4", OutExt: ".webp"}, in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Output, in) {
		t.Fatalf("output = %q, want the input echoed", res.Output)
	}
	if res.Served != 1 || res.SourceCached || res.FetchMs != 0 {
		t.Fatalf("unexpected metrics: %+v", res)
	}
}

func TestEncodeRejectsBadToken(t *testing.T) {
	c, done := newTestWorker(t, nil)
	defer done()
	c.Token = "wrong"

	_, err := c.Encode(context.Background(), Spec{InExt: ".mp4", OutExt: ".mp4"}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v, want 401", err)
	}
}

func TestEncodeSurfacesFFmpegStderr(t *testing.T) {
	c, done := newTestWorker(t, nil)
	defer done()

	_, err := c.Encode(context.Background(), Spec{Args: []string{"FAIL"}, InExt: ".mp4", OutExt: ".mp4"}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "boom: bad input") {
		t.Fatalf("err = %v, want ffmpeg's stderr", err)
	}
}

func TestEncodeSourceKeyFetchesOnce(t *testing.T) {
	src := &fakeSources{data: map[string][]byte{"pbc_1/rec/clip.mp4": []byte("source bytes")}}
	c, done := newTestWorker(t, src)
	defer done()

	spec := Spec{InExt: ".mp4", OutExt: ".mp4", SourceKey: "pbc_1/rec/clip.mp4"}
	first, err := c.Encode(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.Encode(context.Background(), spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Output) != "source bytes" || string(second.Output) != "source bytes" {
		t.Fatalf("outputs = %q, %q", first.Output, second.Output)
	}
	if first.SourceCached || !second.SourceCached {
		t.Fatalf("cached = %v, %v; want false, true", first.SourceCached, second.SourceCached)
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("fetched %d times, want 1", n)
	}
}

func TestEncodeSourceKeyMissing(t *testing.T) {
	c, done := newTestWorker(t, &fakeSources{data: map[string][]byte{}})
	defer done()

	_, err := c.Encode(context.Background(), Spec{InExt: ".mp4", OutExt: ".mp4", SourceKey: "nope"}, nil)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want 502", err)
	}
}

func TestSpecValidate(t *testing.T) {
	for _, s := range []Spec{
		{InExt: "mp4", OutExt: ".mp4"},
		{InExt: ".mp4", OutExt: "/../x"},
		{InExt: ".mp4", OutExt: ".mp4", Args: []string{"-i", "/etc/passwd"}},
		{InExt: ".mp4", OutExt: ".mp4", PreInput: []string{"-i", "x"}},
	} {
		if s.Validate() == nil {
			t.Errorf("Validate(%+v) = nil, want an error", s)
		}
	}
}

// flaky answers the first n requests the way a platform does while the worker
// boots — status, without the worker's marker header — then serves the worker.
func flaky(t *testing.T, n int, status int) (*Client, *atomic.Int32, func()) {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(bin, []byte(fakeFFmpeg), 0o755); err != nil {
		t.Fatal(err)
	}
	worker := (&Server{Token: "secret", FFmpeg: bin, WorkDir: dir}).Handler()
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) <= int32(n) {
			io.Copy(io.Discard, r.Body)
			http.Error(w, "application failed to respond", status)
			return
		}
		worker.ServeHTTP(w, r)
	}))
	return &Client{URL: ts.URL, Token: "secret", HTTP: ts.Client()}, &hits, ts.Close
}

func fastWake(t *testing.T) {
	old := wakeBackoff
	wakeBackoff = 10 * time.Millisecond
	t.Cleanup(func() { wakeBackoff = old })
}

func TestEncodeRetriesWhileWaking(t *testing.T) {
	fastWake(t)
	c, hits, done := flaky(t, 2, http.StatusBadGateway)
	defer done()

	in := []byte("streamed body, sent again on every attempt")
	res, err := c.Encode(context.Background(), Spec{InExt: ".mp4", OutExt: ".mp4"}, in)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Output, in) {
		t.Fatalf("output = %q, want the input echoed", res.Output)
	}
	if n := hits.Load(); n != 3 {
		t.Fatalf("%d requests, want 3", n)
	}
}

func TestHealthRetriesRefusedConnection(t *testing.T) {
	fastWake(t)
	// Reserve a port, free it, and only start serving on it a moment later.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	go func() {
		time.Sleep(100 * time.Millisecond)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return // the test then fails on WakeTimeout
		}
		srv := &http.Server{Handler: (&Server{Token: "secret", WorkDir: t.TempDir()}).Handler()}
		t.Cleanup(func() { srv.Close() })
		srv.Serve(ln)
	}()

	c := &Client{URL: "http://" + addr, Token: "secret", WakeTimeout: 5 * time.Second}
	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
}

func TestEncodeGivesUpAfterWakeTimeout(t *testing.T) {
	fastWake(t)
	c, _, done := flaky(t, 1000, http.StatusServiceUnavailable)
	defer done()
	c.WakeTimeout = 100 * time.Millisecond

	_, err := c.Encode(context.Background(), Spec{InExt: ".mp4", OutExt: ".mp4"}, []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("err = %v, want the platform's 503", err)
	}
}

func TestEncodeDoesNotRetryWorkerErrors(t *testing.T) {
	fastWake(t)
	src := &fakeSources{data: map[string][]byte{}}
	c, done := newTestWorker(t, src)
	defer done()

	// The worker's own 502 (source missing in R2) is an answer, not a boot.
	_, err := c.Encode(context.Background(), Spec{InExt: ".mp4", OutExt: ".mp4", SourceKey: "nope"}, nil)
	if err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("err = %v, want 502", err)
	}
	if n := src.calls.Load(); n != 1 {
		t.Fatalf("fetched %d times, want 1 (no retry)", n)
	}
}
