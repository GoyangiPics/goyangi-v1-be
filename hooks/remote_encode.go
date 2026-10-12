package hooks

// EXPERIMENT (exp branch): run ffmpeg on a pay-per-use encode worker
// (cmd/encode-worker, deployed to Railway) instead of on this server, and log
// what each encode costs in time and money. See docs/encode-worker-experiment.md.
//
// ENCODE_WORKER_URL turns it on; unset, every encode runs locally as on main
// and is still timed, so the same burst can be run both ways and compared.

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"goyangi-v1-be/remoteenc"
)

// remoteEncoder is the worker client when ENCODE_WORKER_URL is set, nil
// otherwise.
var remoteEncoder = sync.OnceValue(func() *remoteenc.Client {
	url := strings.TrimRight(os.Getenv("ENCODE_WORKER_URL"), "/")
	if url == "" {
		return nil
	}
	token := os.Getenv("ENCODE_WORKER_TOKEN")
	if token == "" {
		log.Printf("❌ encode: ENCODE_WORKER_URL is set but ENCODE_WORKER_TOKEN isn't — encoding locally")
		return nil
	}
	log.Printf("🛰️  encode: ffmpeg runs on the worker at %s", url)
	return &remoteenc.Client{URL: url, Token: token, HTTP: &http.Client{}}
})

// Railway's usage rates: $20 per vCPU-month and $10 per GB-month, metered by
// the minute. Override if they change.
var (
	vcpuMinuteUSD = envFloat("RAILWAY_VCPU_MIN_USD", 0.000463)
	gbMinuteUSD   = envFloat("RAILWAY_GB_MIN_USD", 0.000231)
)

func envFloat(name string, def float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(name), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

// remoteSources maps an upload's source bytes (by the address of their first
// byte) to the R2 key they were read from, so runFFmpegRemote can hand the
// worker the key instead of pushing ~100 MB through this server's connection
// once per rendition. Anything not registered — /api/convert inputs, the
// intermediate MP4 the previews are cut from — goes in the request body.
var remoteSources sync.Map // *byte → string

// registerRemoteSource records content's R2 key until the returned func runs.
func registerRemoteSource(content []byte, key string) (unregister func()) {
	if remoteEncoder() == nil || len(content) == 0 {
		return func() {}
	}
	p := &content[0]
	remoteSources.Store(p, key)
	return func() { remoteSources.Delete(p) }
}

func remoteSourceKey(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	if key, ok := remoteSources.Load(&content[0]); ok {
		return key.(string)
	}
	return ""
}

var specExtRe = regexp.MustCompile(`^\.[a-z0-9]{1,5}$`)

func runFFmpegRemote(c *remoteenc.Client, content []byte, srcExt, outExt string, preInput, args []string) ([]byte, error) {
	op := encodeOpName()
	inExt := strings.ToLower(srcExt)
	if !specExtRe.MatchString(inExt) {
		inExt = ".bin" // ffmpeg probes the content; the name is only a hint
	}
	spec := remoteenc.Spec{
		PreInput:  preInput,
		Args:      args,
		InExt:     inExt,
		OutExt:    outExt,
		SourceKey: remoteSourceKey(content),
	}
	// encodeTimeout bounds ffmpeg on the worker; the slack covers moving the
	// input and output and a cold start.
	ctx, cancel := context.WithTimeout(context.Background(), encodeTimeout+2*time.Minute)
	defer cancel()
	res, err := c.Encode(ctx, spec, content)
	if err != nil {
		log.Printf("📊 encode[remote] %s failed: %v", op, firstLine(err))
		return nil, err
	}
	logRemoteEncode(op, spec, len(content), res)
	return res.Output, nil
}

// encodeTotals accumulates the session's encodes for the running summary.
var encodeTotals struct {
	sync.Mutex
	remoteN, localN           int
	remoteCPUMs, localCPUMs   int64
	remoteWallMs, localWallMs int64
	remoteUSD                 float64
}

func logRemoteEncode(op string, spec remoteenc.Spec, inLen int, r *remoteenc.Result) {
	rssGB := float64(r.MaxRSSKB) / (1 << 20)
	// Memory is charged as if the peak were held for the whole run — an upper
	// bound; the CPU part is exact.
	usd := float64(r.CPUMs)/60000*vcpuMinuteUSD + rssGB*float64(r.WorkerMs)/60000*gbMinuteUSD

	src := "body " + fmtSize(inLen)
	if spec.SourceKey != "" {
		src = "r2"
		if r.SourceCached {
			src = "r2 (cached)"
		}
	}
	cold := ""
	if r.Served == 1 {
		cold = " COLD"
	}
	// Whatever the worker didn't spend fetching or encoding: moving the input
	// and output, waiting for a job slot, and booting when cold.
	overhead := r.TotalMs - r.WorkerMs - r.FetchMs

	encodeTotals.Lock()
	encodeTotals.remoteN++
	encodeTotals.remoteCPUMs += r.CPUMs
	encodeTotals.remoteWallMs += r.WorkerMs
	encodeTotals.remoteUSD += usd
	n, cpu, total := encodeTotals.remoteN, encodeTotals.remoteCPUMs, encodeTotals.remoteUSD
	encodeTotals.Unlock()

	log.Printf("📊 encode[remote] %s%s src=%s out=%s total=%s worker=%s fetch=%s overhead=%s cpu=%s (%.1f cores) rss=%.2fGB ≈$%.5f · session: %d remote, cpu %s, ≈$%.4f",
		op, cold, src, fmtSize(len(r.Output)),
		fmtMs(r.TotalMs), fmtMs(r.WorkerMs), fmtMs(r.FetchMs), fmtMs(overhead),
		fmtMs(r.CPUMs), cores(r.CPUMs, r.WorkerMs), rssGB, usd,
		n, fmtMs(cpu), total)
}

func logLocalEncode(op string, wall time.Duration, ps *os.ProcessState) {
	wallMs := wall.Milliseconds()
	var cpuMs int64
	if ps != nil {
		cpuMs = (ps.UserTime() + ps.SystemTime()).Milliseconds()
	}

	encodeTotals.Lock()
	encodeTotals.localN++
	encodeTotals.localCPUMs += cpuMs
	encodeTotals.localWallMs += wallMs
	n, cpu, wallTotal := encodeTotals.localN, encodeTotals.localCPUMs, encodeTotals.localWallMs
	encodeTotals.Unlock()

	log.Printf("📊 encode[local] %s wall=%s cpu=%s (%.1f cores) rss=%.2fGB · session: %d local, cpu %s, wall %s",
		op, fmtMs(wallMs), fmtMs(cpuMs), cores(cpuMs, wallMs),
		float64(remoteenc.MaxRSSKB(ps))/(1<<20), n, fmtMs(cpu), fmtMs(wallTotal))
}

// encodeOpName is the pipeline function that asked for this encode
// (transcodeGifToAV1Tuned, encodeH264CPU, …), for the log lines.
func encodeOpName() string {
	pcs := make([]uintptr, 10)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		f, more := frames.Next()
		name := f.Function[strings.LastIndex(f.Function, ".")+1:]
		switch name {
		case "", "runFFmpeg", "runFFmpegWith", "runFFmpegRemote":
		default:
			return name
		}
		if !more {
			return "ffmpeg"
		}
	}
}

func cores(cpuMs, wallMs int64) float64 {
	if wallMs <= 0 {
		return 0
	}
	return float64(cpuMs) / float64(wallMs)
}

func fmtMs(ms int64) string {
	return fmt.Sprintf("%.1fs", float64(ms)/1000)
}

func fmtSize(n int) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%dKB", n/1024)
}
