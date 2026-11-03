package remoteenc

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// SourceFetcher copies an R2 object into w.
type SourceFetcher interface {
	Fetch(ctx context.Context, key string, w io.Writer) error
}

// Server is the worker side of the protocol.
type Server struct {
	Token   string
	FFmpeg  string        // binary to run; "ffmpeg" when empty
	Sources SourceFetcher // nil: SourceKey specs are refused
	MaxJobs int           // concurrent ffmpeg runs; 4 when ≤0
	Timeout time.Duration // per ffmpeg run; 5 minutes when 0, same as the backend's encodeTimeout
	WorkDir string        // temp root; os.TempDir() when empty
	Health  func() any    // extra fields for GET /healthz

	once   sync.Once
	sem    chan struct{}
	cache  *sourceCache
	served atomic.Int64
}

// maxBody caps one request. Uploads are ≤100 MB; the slack is for multipart
// framing and the odd oversized source.
const maxBody = 512 << 20

func (s *Server) init() {
	s.once.Do(func() {
		if s.FFmpeg == "" {
			s.FFmpeg = "ffmpeg"
		}
		if s.MaxJobs <= 0 {
			s.MaxJobs = 4
		}
		if s.Timeout == 0 {
			s.Timeout = 5 * time.Minute
		}
		if s.WorkDir == "" {
			s.WorkDir = os.TempDir()
		}
		s.sem = make(chan struct{}, s.MaxJobs)
		s.cache = &sourceCache{dir: s.WorkDir, entries: map[string]*sourceEntry{}}
	})
}

// Handler serves GET /healthz and POST /encode.
func (s *Server) Handler() http.Handler {
	s.init()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("POST /encode", s.encode)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(HeaderWorker, "1")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) authorized(r *http.Request) bool {
	want := "Bearer " + s.Token
	got := r.Header.Get("Authorization")
	return s.Token != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		// Unauthenticated callers (the platform's health check) learn only
		// that the process is up.
		w.Write([]byte("ok\n"))
		return
	}
	out := map[string]any{"served": s.served.Load(), "maxJobs": s.MaxJobs}
	if s.Health != nil {
		out["worker"] = s.Health()
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (s *Server) encode(w http.ResponseWriter, r *http.Request) {
	served := s.served.Add(1)
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)

	job, err := os.MkdirTemp(s.WorkDir, "enc-*")
	if err != nil {
		http.Error(w, "work dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.RemoveAll(job)

	spec, inPath, err := s.readRequest(r, job)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var fetchMs int64
	cached := false
	if spec.SourceKey != "" {
		if s.Sources == nil {
			http.Error(w, "this worker has no R2 access; send the input in the body", http.StatusBadRequest)
			return
		}
		path, ms, hit, release, err := s.cache.get(r.Context(), s.Sources, spec.SourceKey, spec.InExt)
		if err != nil {
			http.Error(w, "fetch source: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer release()
		inPath, fetchMs, cached = path, ms, hit
	}

	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-r.Context().Done():
		return
	}

	outPath := filepath.Join(job, "out"+spec.OutExt)
	args := append([]string{}, spec.PreInput...)
	args = append(args, "-i", inPath)
	args = append(args, spec.Args...)
	args = append(args, "-y", outPath)

	ctx, cancel := context.WithTimeout(r.Context(), s.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.FFmpeg, args...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	start := time.Now()
	runErr := cmd.Run()
	workerMs := time.Since(start).Milliseconds()

	if runErr != nil {
		if ctx.Err() == context.DeadlineExceeded {
			http.Error(w, fmt.Sprintf("ffmpeg timed out after %s", s.Timeout), http.StatusGatewayTimeout)
			return
		}
		http.Error(w, fmt.Sprintf("%v\n%s", runErr, stderr.String()), http.StatusUnprocessableEntity)
		return
	}

	out, err := os.ReadFile(outPath)
	if err != nil {
		http.Error(w, "read output: "+err.Error(), http.StatusInternalServerError)
		return
	}

	ps := cmd.ProcessState
	cpuMs := (ps.UserTime() + ps.SystemTime()).Milliseconds()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.Itoa(len(out)))
	h.Set(HeaderWorkerMs, strconv.FormatInt(workerMs, 10))
	h.Set(HeaderFetchMs, strconv.FormatInt(fetchMs, 10))
	h.Set(HeaderCPUMs, strconv.FormatInt(cpuMs, 10))
	h.Set(HeaderMaxRSSKB, strconv.FormatInt(MaxRSSKB(ps), 10))
	h.Set(HeaderServed, strconv.FormatInt(served, 10))
	if cached {
		h.Set(HeaderSourceCached, "1")
	}
	w.Write(out)

	log.Printf("encode #%d: %s→%s in=%s wall=%dms cpu=%dms fetch=%dms cached=%v out=%dKB",
		served, spec.InExt, spec.OutExt, sourceLabel(spec), workerMs, cpuMs, fetchMs, cached, len(out)/1024)
}

func sourceLabel(s Spec) string {
	if s.SourceKey != "" {
		return "r2"
	}
	return "body"
}

// readRequest parses the multipart body: the spec first, then the input (if
// any) streamed straight to disk.
func (s *Server) readRequest(r *http.Request, job string) (Spec, string, error) {
	var spec Spec
	mr, err := r.MultipartReader()
	if err != nil {
		return spec, "", err
	}
	gotSpec := false
	inPath := ""
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return spec, "", err
		}
		switch part.FormName() {
		case "spec":
			if err := json.NewDecoder(io.LimitReader(part, 1<<20)).Decode(&spec); err != nil {
				return spec, "", fmt.Errorf("spec: %w", err)
			}
			if err := spec.Validate(); err != nil {
				return spec, "", err
			}
			gotSpec = true
		case "input":
			if !gotSpec {
				return spec, "", fmt.Errorf("spec must precede input")
			}
			inPath = filepath.Join(job, "in"+spec.InExt)
			f, err := os.Create(inPath)
			if err != nil {
				return spec, "", err
			}
			_, err = io.Copy(f, part)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return spec, "", fmt.Errorf("input: %w", err)
			}
		}
		part.Close()
	}
	if !gotSpec {
		return spec, "", fmt.Errorf("missing spec")
	}
	if spec.SourceKey == "" && inPath == "" {
		return spec, "", fmt.Errorf("missing input")
	}
	return spec, inPath, nil
}

// tailBuffer keeps the last 8 KB written — the end of ffmpeg's stderr is
// where the actual error is.
type tailBuffer struct{ b []byte }

const tailMax = 8 << 10

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailMax {
		t.b = append(t.b[:0], t.b[len(t.b)-tailMax:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(bytes.TrimSpace(t.b)) }

// sourceCache keeps fetched R2 sources on disk for a few minutes. One upload
// fans out into several encodes of the same source (the AV1 MP4 and the H.264
// SD run concurrently), and each would otherwise fetch it again. Concurrent
// requests for one key share a single fetch.
type sourceCache struct {
	dir     string
	mu      sync.Mutex
	entries map[string]*sourceEntry
}

type sourceEntry struct {
	ready    chan struct{}
	path     string
	fetchMs  int64
	err      error
	refs     int
	lastUsed time.Time
}

const (
	sourceTTL        = 5 * time.Minute
	sourceMaxEntries = 24
)

// get returns a local path for key, fetching it if needed, and a release func
// the caller runs once ffmpeg is done with the file. fetchMs is 0 on a hit.
func (c *sourceCache) get(ctx context.Context, f SourceFetcher, key, ext string) (path string, fetchMs int64, hit bool, release func(), err error) {
	c.mu.Lock()
	c.evictLocked()
	e, ok := c.entries[key]
	if !ok {
		e = &sourceEntry{ready: make(chan struct{})}
		c.entries[key] = e
	}
	e.refs++
	c.mu.Unlock()

	release = func() {
		c.mu.Lock()
		e.refs--
		e.lastUsed = time.Now()
		c.mu.Unlock()
	}

	if !ok {
		// Fetched detached from this request's context: other requests may be
		// waiting on the same entry, and one caller giving up shouldn't fail
		// them all.
		e.path, e.fetchMs, e.err = c.fetch(f, key, ext)
		close(e.ready)
		if e.err != nil {
			c.mu.Lock()
			delete(c.entries, key)
			c.mu.Unlock()
		}
	} else {
		select {
		case <-e.ready:
		case <-ctx.Done():
			release()
			return "", 0, false, nil, ctx.Err()
		}
	}
	if e.err != nil {
		release()
		return "", 0, false, nil, e.err
	}
	if ok {
		return e.path, 0, true, release, nil
	}
	return e.path, e.fetchMs, false, release, nil
}

func (c *sourceCache) fetch(f SourceFetcher, key, ext string) (string, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	file, err := os.CreateTemp(c.dir, "src-*"+ext)
	if err != nil {
		return "", 0, err
	}
	start := time.Now()
	err = f.Fetch(ctx, key, file)
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(file.Name())
		return "", 0, err
	}
	return file.Name(), time.Since(start).Milliseconds(), nil
}

// evictLocked drops idle entries past their TTL, then the oldest idle ones
// while over the size cap.
func (c *sourceCache) evictLocked() {
	now := time.Now()
	drop := func(k string, e *sourceEntry) {
		os.Remove(e.path)
		delete(c.entries, k)
	}
	idle := func(e *sourceEntry) bool {
		select {
		case <-e.ready:
			return e.refs == 0
		default:
			return false
		}
	}
	for k, e := range c.entries {
		if idle(e) && now.Sub(e.lastUsed) > sourceTTL {
			drop(k, e)
		}
	}
	for len(c.entries) > sourceMaxEntries {
		oldestKey := ""
		var oldest *sourceEntry
		for k, e := range c.entries {
			if idle(e) && (oldest == nil || e.lastUsed.Before(oldest.lastUsed)) {
				oldestKey, oldest = k, e
			}
		}
		if oldest == nil {
			return
		}
		drop(oldestKey, oldest)
	}
}
