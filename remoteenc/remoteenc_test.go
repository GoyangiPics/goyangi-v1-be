//go:build unix

package remoteenc

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
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
