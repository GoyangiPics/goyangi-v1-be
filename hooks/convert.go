package hooks

import (
	"errors"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// RegisterConvertRoutes adds stateless one-shot conversion endpoints.
// Files are processed in-memory and returned in the HTTP response — nothing
// is persisted to disk, R2, or the database.
//
// Currently exposes:
//
//	POST /api/convert/avif   multipart "file" → image/avif body
//	POST /api/convert/webp   multipart "file" → image/webp body
//
// Both share one handler (convertPreview); the animated preview format is the
// only thing that differs. Future endpoints (mp4, static thumb, etc.) can live
// alongside.
//
// Deliberately unauthenticated — the public gif-converter tool page is the
// consumer. The request gate and size cap below are what bound anonymous
// traffic; for per-IP limits add a rule in the admin UI under Settings ->
// Rate limits labelled "POST /api/convert/" (the trailing slash makes it a
// prefix rule) — custom routes inherit the global rate-limit middleware.
func RegisterConvertRoutes(app *pocketbase.PocketBase) {
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		e.Router.POST("/api/convert/avif", convertPreview(ConvertAVIF, "image/avif"))
		e.Router.POST("/api/convert/webp", convertPreview(ConvertWebP, "image/webp"))
		return e.Next()
	})
}

// maxConvertUploadSize is this route's own per-file ceiling — 100 MB, matching
// what Cloudflare lets through to the origin (and the FE tool's cap), NOT the
// 256 MB defense-in-depth bound uploads get. The distinction matters because a
// convert request holds its file fully in RAM while it waits for an encode
// slot; the upload pipeline holds at most one file at a time.
const maxConvertUploadSize int64 = 100 * 1024 * 1024

// convertGate bounds how many convert requests may hold a request body at all.
// Without it, N anonymous requests each buffered their file and then queued up
// to 30 s on the shared encode semaphore — N × 100 MB of RAM parked on a
// public endpoint. Full gate → immediate 503, so a burst fails fast instead of
// stacking. Sized by MAX_CONVERT_REQUESTS (default 2: one encoding, one ready).
var convertGate = make(chan struct{}, envInt("MAX_CONVERT_REQUESTS", 2))

// convertPreview returns a handler that accepts a multipart upload under the
// "file" field, encodes it to an animated preview of the given kind using the
// same settings as the upload pipeline's preview generator, and streams the
// result back to the client.
//
// Tuning note: this reuses the upload pipeline's encoders (generateAnimatedAVIF
// / generateAnimatedWebP) for parity. If the convert endpoint ever needs its
// own tuning (e.g. a different resolution cap or quality target), extract
// dedicated encoder functions rather than diverging these — the upload pipeline
// depends on their current behavior.
func convertPreview(kind ConvertKind, contentType string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		start := time.Now()

		// A supervised restart is a few seconds away (hooks/deploy.go).
		if draining.Load() {
			return apis.NewApiError(503, "Server is updating, please retry in a minute.", nil)
		}

		// Taken before the body is even parsed and held until the response is
		// written, so the whole buffered-file lifetime is bounded — see
		// convertGate.
		select {
		case convertGate <- struct{}{}:
			defer func() { <-convertGate }()
		default:
			return apis.NewApiError(503, "Server busy, please retry shortly", nil)
		}

		files, err := e.FindUploadedFiles("file")
		if err != nil {
			return apis.NewBadRequestError("Missing multipart 'file' field", err)
		}
		if len(files) == 0 {
			return apis.NewBadRequestError("No file uploaded", nil)
		}
		f := files[0]

		if f.Size > maxConvertUploadSize {
			return apis.NewBadRequestError(
				fmt.Sprintf("File %q is %.1f MB, exceeds the %d MB limit",
					f.Name,
					float64(f.Size)/(1024*1024),
					maxConvertUploadSize/(1024*1024)),
				nil,
			)
		}

		reader, err := f.Reader.Open()
		if err != nil {
			return apis.NewBadRequestError("Cannot open uploaded file", err)
		}
		defer reader.Close()

		content, err := io.ReadAll(reader)
		if err != nil {
			return apis.NewBadRequestError("Cannot read uploaded file", err)
		}

		srcExt := filepath.Ext(f.Name)
		if srcExt == "" {
			srcExt = ".mp4"
		}

		// Convert gates through the shared process semaphore so this public
		// endpoint can't spawn unbounded concurrent ffmpeg jobs (it shares the
		// CPU/GPU budget with the upload pipeline), rejecting when busy.
		outBytes, outExt, err := Convert(kind, content, srcExt)
		if errors.Is(err, ErrBusy) {
			return apis.NewApiError(503, "Server busy, please retry shortly", nil)
		}
		if err != nil {
			LogUploadError(fmt.Sprintf("convert/%s: encode failed for %s: %v", kind, f.Name, err),
				map[string]any{"endpoint": "/api/convert/" + string(kind), "filename": f.Name})
			return apis.NewInternalServerError(fmt.Sprintf("%s encoding failed", strings.ToUpper(string(kind))), err)
		}

		log.Printf("✅ convert/%s: %s (%.1f KB) → %s (%.1f KB) in %v",
			kind,
			f.Name,
			float64(f.Size)/1024,
			strings.ToUpper(string(kind)),
			float64(len(outBytes))/1024,
			time.Since(start).Round(time.Millisecond),
		)

		h := e.Response.Header()
		h.Set("Content-Type", contentType)
		h.Set("Content-Length", strconv.Itoa(len(outBytes)))
		h.Set("Content-Disposition", fmt.Sprintf(`inline; filename="preview%s"`, outExt))
		h.Set("Cache-Control", "no-store")

		_, err = e.Response.Write(outBytes)
		return err
	}
}
