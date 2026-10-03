package hooks

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// Pixel dimensions on content records, so the frontend can reserve the right box
// before a byte of media has loaded.
//
// Without these every card is laid out at whatever height the media happens to
// have once it arrives: the masonry grid reflows on first paint, and stepping
// through a set whose items have different aspect ratios makes the whole column
// jump. The frontend cannot know an aspect ratio ahead of time — that is exactly
// what the browser is waiting for the file to tell it — so it has to come from
// here, where ffprobe is already a dependency.
//
// What they describe: the DISPLAY aspect of the rendition the frontend renders.
// Only the ratio is guaranteed exact; see setRecordDimensions for which bytes
// each filetype is measured from and why.

// probeDimensions returns the pixel width and height of the first video stream
// (or still image) in content.
//
// A separate ffprobe pass rather than parsing the encoder's stderr: the probe
// runs in tens of milliseconds against a multi-second encode, and reading it out
// of ffmpeg's log output would couple this to the exact phrasing of a build.
func probeDimensions(content []byte, ext string) (int, int, error) {
	if ext == "" {
		ext = ".mp4"
	}

	input, err := os.CreateTemp("", "goyangi-probe-*"+ext)
	if err != nil {
		return 0, 0, fmt.Errorf("create temp input: %w", err)
	}
	defer os.Remove(input.Name())
	if _, err := input.Write(content); err != nil {
		input.Close()
		return 0, 0, fmt.Errorf("write temp input: %w", err)
	}
	input.Close()

	// csv=p=0 gives a bare "1920,1080" — the same shape probeFpsAndFrameCount
	// parses. `stream=` (not `format=`) because dimensions live on the stream.
	out, err := runProbe("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height",
		"-of", "csv=p=0",
		input.Name(),
	)
	if err != nil {
		return 0, 0, err
	}

	return parseDimensions(string(out))
}

// parseDimensions reads ffprobe's `csv=p=0` width/height output.
//
// Split out to be testable without ffmpeg on the box, and because the output is
// less predictable than it looks: some builds emit a trailing comma, and a file
// with several video streams emits one line per stream even under
// -select_streams v:0.
func parseDimensions(out string) (int, int, error) {
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		fields := strings.Split(strings.TrimSpace(line), ",")
		if len(fields) < 2 {
			continue
		}
		w, wErr := strconv.Atoi(strings.TrimSpace(fields[0]))
		h, hErr := strconv.Atoi(strings.TrimSpace(fields[1]))
		// Zero is not a usable dimension and neither is a negative one; both mean
		// the probe found a stream but not its size, which must not be stored as
		// if it were a real measurement.
		if wErr != nil || hErr != nil || w <= 0 || h <= 0 {
			continue
		}
		return w, h, nil
	}
	return 0, 0, fmt.Errorf("no usable dimensions in probe output %q", out)
}

// stickerDimensions is what generateStickerAVIF always produces: its filter
// chain is crop='min(iw,ih)':'min(iw,ih)',scale=200:200, so the output is a
// 200×200 square whatever went in. Known statically, so there is nothing to
// probe — and it is the one filetype whose SOURCE dimensions would be wrong,
// since the crop discards the original aspect ratio entirely.
const stickerDimensions = 200

// RegisterDimensionFields ensures the two fields exist, once, at boot.
//
// After e.Next() and non-fatal, matching RegisterBackfills and
// RegisterSystemLogs: collections can only be queried once the DB is up, and a
// failure here must not stop the app serving — uploads still work, they just
// store no dimensions and the frontend falls back to measuring.
func RegisterDimensionFields(app *pocketbase.PocketBase) {
	app.OnBootstrap().BindFunc(func(e *core.BootstrapEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		if err := ensureDimensionFields(app); err != nil {
			log.Printf("⚠️  dimensions: could not ensure contents.width/height: %v", err)
		}
		return nil
	})
}

// ensureDimensionFields adds `width` and `height` to the contents collection if
// they aren't there yet.
//
// Programmatic rather than a manual admin-UI step, following
// ensureSystemLogsCollection: this project has no migrations directory, so a
// deploy that needed someone to add two fields by hand first would write zeroes
// for every upload until they did. Idempotent and guarded, so it is a no-op on
// every boot after the first.
//
// pb_schema.json carries the same two fields — it is a hand-maintained export,
// and leaving it behind would mean the next `pnpm typegen` in the frontend
// deleted the types this feature depends on.
func ensureDimensionFields(app *pocketbase.PocketBase) error {
	collection, err := app.FindCollectionByNameOrId("contents")
	if err != nil {
		return fmt.Errorf("find contents: %w", err)
	}

	added := false
	for _, name := range []string{"width", "height"} {
		if collection.Fields.GetByName(name) != nil {
			continue
		}
		// Deliberately not Required, and with no Min/Max validator: existing
		// records have no dimensions until the backfill runs (see
		// scripts/backfilldims), and a required field would reject every
		// unrelated update to one of them in the meantime.
		//
		// OnlyInt to match pb_schema.json — pixels are whole numbers, and a
		// mismatch here would mean the next schema import silently changed the
		// field this code created.
		collection.Fields.Add(&core.NumberField{Name: name, OnlyInt: true})
		added = true
	}
	if !added {
		return nil
	}

	return app.Save(collection)
}

// setRecordDimensions stamps width/height on the record, best-effort.
//
// Best-effort on purpose: a failed probe must not fail an upload whose media
// encoded fine. The frontend treats 0 as "unknown" and falls back to measuring
// the media once it loads, which is what it did for every record before this
// existed.
//
// `content` must be bytes whose display aspect matches the rendition the
// frontend renders — see the call sites. For stills that is deliberately the
// SOURCE rather than the AVIF output: stillScaleCap caps the longer edge without
// cropping, so the ratio is identical, and the source is already in memory. The
// stored numbers are then the source's pixel dimensions rather than the AVIF's,
// which is why only the RATIO is documented as exact.
func setRecordDimensions(record *core.Record, content []byte, ext, recordId string) {
	w, h, err := probeDimensions(content, ext)
	if err != nil {
		LogUploadWarning(
			fmt.Sprintf("R2: could not probe dimensions for %s: %v", recordId, err),
			map[string]any{"record": recordId},
		)
		return
	}
	record.Set("width", w)
	record.Set("height", h)
}
