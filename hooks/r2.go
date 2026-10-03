package hooks

import (
	crand "crypto/rand"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

const (
	idolCollectionId  = "pbc_2746231271"
	groupCollectionId = "pbc_3346940990"
)

// processSem limits how many files can be transcoded at the same time.
// Default is 1 (one at a time) to avoid saturating CPU/memory on large uploads.
// Override by setting MAX_PROCESS_JOBS env var.
var processSem = func() chan struct{} {
	n := 1
	if v := os.Getenv("MAX_PROCESS_JOBS"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	log.Printf("🔧 R2 processing queue: %d concurrent job(s)", n)
	return make(chan struct{}, n)
}()

// AvifInfo carries all metadata for a completed AVIF.
type AvifInfo struct {
	AvifURL  string
	Title    string
	Idol     string
	Group    string
	Uploader string
	Date     string // YYMMDD
	Source   string
}

// OnAvifReady is called once per file after the AVIF is in R2.
// Wire it from main.go to avoid a hooks → bot import cycle.
var OnAvifReady func(info AvifInfo)

// OnContentPublished is called once per content record after every rendition is
// in R2 AND the record has been saved.
//
// This is deliberately a separate hook from OnAvifReady, which fires mid-
// processing — before app.Save, so the original/preview/static fields are not
// yet persisted. Anything that reads the record back out of the database (the
// Discord upload announcement does) must hang off this one or it will see the
// record without its URLs.
//
// Wire it from main.go to avoid a hooks → bot import cycle.
var OnContentPublished func(recordID string)

// randomSuffix returns 8 hex chars (32 bits) of crypto-random entropy for
// content-id uniqueness. Wider + crypto-grade vs the old 16-bit math/rand.
func randomSuffix() string {
	b := make([]byte, 4)
	if _, err := crand.Read(b); err != nil {
		panic(fmt.Errorf("crypto/rand failed: %w", err))
	}
	return fmt.Sprintf("%x", b)
}

// contentNaming carries the slugified group/idol names and date shared by
// record-id generation and R2 object keys.
type contentNaming struct {
	groupNames, idolNames     []string
	groupsJoined, idolsJoined string // "unknown" when empty
	dateStr                   string // YYMMDD
}

func namingFor(app *pocketbase.PocketBase, record *core.Record) contentNaming {
	n := contentNaming{
		groupNames: resolveAllRelationNames(app, record.GetStringSlice("group"), groupCollectionId),
		idolNames:  resolveAllRelationNames(app, record.GetStringSlice("idol"), idolCollectionId),
		dateStr:    recordDateString(record),
	}
	n.groupsJoined = strings.Join(n.groupNames, "-")
	n.idolsJoined = strings.Join(n.idolNames, "-")
	if n.groupsJoined == "" {
		n.groupsJoined = "unknown"
	}
	if n.idolsJoined == "" {
		n.idolsJoined = "unknown"
	}
	return n
}

func generateContentId(app *pocketbase.PocketBase, record *core.Record) string {
	n := namingFor(app, record)
	suffix := randomSuffix()

	if len(n.groupNames) == 1 && len(n.idolNames) == 1 {
		return fmt.Sprintf("%s-%s-%s-%s", n.dateStr, n.groupNames[0], n.idolNames[0], suffix)
	} else if len(n.groupNames) == 1 {
		return fmt.Sprintf("%s-%s-%s-%s", n.dateStr, n.groupsJoined, n.idolsJoined, suffix)
	}
	return fmt.Sprintf("mix-%s-%s-%s-%s", n.dateStr, n.groupsJoined, n.idolsJoined, suffix)
}

// notifyAvifReady fires the OnAvifReady callback (if wired) with the record's
// display metadata, for the Discord "AVIF ready" notification.
func notifyAvifReady(app *pocketbase.PocketBase, record *core.Record, url, dateStr string) {
	if OnAvifReady == nil {
		return
	}
	go OnAvifReady(AvifInfo{
		AvifURL:  url,
		Title:    record.GetString("title"),
		Idol:     resolveDisplayName(app, record, "idol", idolCollectionId),
		Group:    resolveDisplayName(app, record, "group", groupCollectionId),
		Uploader: resolveDisplayName(app, record, "uploader", "uploaders"),
		Date:     dateStr,
		Source:   record.GetString("source"),
	})
}

// maxUploadSize is the per-file ceiling enforced at record-create time as a
// defense-in-depth check against FE bypass and against runaway memory usage
// in the encode pipeline. Must be >= the highest FE per-filetype limit
// (uploads.vue uploadTypeConfig) so any FE-accepted upload passes backend
// validation. Tighter per-filetype limits are still enforced FE-side.
//
// The "contents" collection's own file-field maxSize is a separate, independent
// gate — raising this constant alone is not enough, it must be raised in the
// PocketBase admin UI too (see pb_schema.json).
//
// Note this is NOT the Discord attachment ceiling: Discord caps bot uploads at
// 100 MB even in a Boost Level 3 guild (see maxDiscordUpload in bot/convert.go).
const maxUploadSize int64 = 256 * 1024 * 1024 // 256 MB

func RegisterR2Hooks(app *pocketbase.PocketBase) {
	// Fail loudly at boot if required config is missing — otherwise public
	// URLs silently become malformed ("/key") and delete-matching breaks.
	if strings.TrimRight(os.Getenv("R2_PUBLIC_URL"), "/") == "" {
		log.Fatal("❌ R2_PUBLIC_URL environment variable is required")
	}

	app.OnRecordCreate("contents").BindFunc(func(e *core.RecordEvent) error {
		for _, f := range e.Record.GetUnsavedFiles("file") {
			if f.Size > maxUploadSize {
				return fmt.Errorf("file %q is %.1f MB, exceeds the %d MB upload limit",
					f.Name,
					float64(f.Size)/(1024*1024),
					maxUploadSize/(1024*1024),
				)
			}
		}
		// The tab a file was dropped on is a hint; the bytes decide. Before the
		// save, so the queue bucket and the encode branch both see the corrected
		// value. See classify.go.
		reclassifyContentType(e.Record)
		e.Record.Id = generateContentId(app, e.Record)
		return e.Next()
	})

	app.OnRecordCreate("contents_sets").BindFunc(func(e *core.RecordEvent) error {
		e.Record.Id = generateContentId(app, e.Record)
		return e.Next()
	})

	app.OnRecordAfterCreateSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
		if e.Record.GetString("file") == "" {
			return e.Next()
		}
		// Counted here, synchronously, rather than inside the goroutine: the
		// record exists the moment this hook returns, so GET /api/queue has to
		// include the job from that moment too. See enqueueJob.
		release := enqueueJob(e.Record.GetString("filetype"))
		recordId := e.Record.Id
		go func() {
			defer release()
			moveFileToCustomR2Path(app, recordId)
		}()
		return e.Next()
	})

	// Clean up every R2 object a deleted record pointed at.
	//
	// This also covers cascades: contents.set is cascadeDelete, and PocketBase
	// runs cascade deletions through the full ORM, so deleting a set fires this
	// hook once per child. That is why the work goes through a bounded queue
	// instead of a goroutine per URL — a 200-item set would otherwise spawn
	// ~800 goroutines each holding its own S3 client, with nothing waiting on
	// them and all of them lost at shutdown.
	app.OnRecordAfterDeleteSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
		enqueueR2DeleteURLs(e.App, r2URLs(e.Record), e.Record.Id)
		return e.Next()
	})

	// Clean up objects a record has *stopped* pointing at.
	//
	// R2 keys are derived from the record's metadata (see customKey below), so
	// reprocessing with the same metadata overwrites in place and leaks nothing.
	// Keys change when `filetype` is edited (.avif <-> .mp4), `preview_format`
	// is flipped (.avif -> .webp), or idol/group/date are edited before a
	// reprocess — and before this hook existed, the old objects simply stayed.
	if r2UpdateCleanupEnabled {
		app.OnRecordAfterUpdateSuccess("contents").BindFunc(func(e *core.RecordEvent) error {
			// Original() holds the pre-save DB state: originalData is only
			// written by PostScan, which Save never calls.
			stale := staleR2URLs(r2URLs(e.Record.Original()), r2URLs(e.Record))
			if len(stale) > 0 {
				enqueueR2DeleteURLs(e.App, stale, e.Record.Id)
			}
			return e.Next()
		})
	}

	startR2DeleteDispatcher(app)

	app.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		drainR2DeleteQueue(app)
		return e.Next()
	})
}

func moveFileToCustomR2Path(app *pocketbase.PocketBase, recordId string) {
	time.Sleep(1 * time.Second)

	// Acquire processing slot — blocks here if the queue is full.
	// This prevents multiple heavy FFmpeg jobs from running concurrently.
	//
	// Reported from the queue counters rather than len(processSem), which only
	// ever sees the slots in use (≤ MAX_PROCESS_JOBS) and so logged "1/1 in use"
	// whether one job was waiting or thirty. This job is itself counted in
	// Waiting at this point, hence the -1.
	if snap := queueSnapshot(); snap.Waiting > 1 {
		log.Printf("⏳ R2: record %s waiting for a processing slot (%d ahead, %d/%d slots in use)…",
			recordId, snap.Waiting-1, snap.Active, snap.Capacity)
	}
	processSem <- struct{}{}
	releaseActive := beginActive()
	defer func() {
		releaseActive()
		<-processSem
	}()
	log.Printf("⚡ R2: record %s acquired processing slot", recordId)

	record, err := app.FindRecordById("contents", recordId)
	if err != nil {
		LogUploadError(fmt.Sprintf("R2: record %s not found: %v", recordId, err), map[string]any{"record": recordId})
		return
	}

	filename := record.GetString("file")
	if filename == "" {
		return
	}

	n := namingFor(app, record)
	ext := filepath.Ext(filename)
	// Provenance stamped into every MP4 rendition — see filemeta.go.
	meta := metadataArgs(renditionTags(app, record, n))

	shortId := recordId
	if len(shortId) > 4 {
		shortId = shortId[len(shortId)-4:]
	}

	// R2 keys are namespaced by group/idol when unambiguous, else mix/.
	var customKey string
	if record.GetString("filetype") == "sticker" {
		customKey = fmt.Sprintf("v1/stickers/%s-%s-%s-%s%s", n.dateStr, n.groupsJoined, n.idolsJoined, shortId, ext)
	} else if len(n.groupNames) == 1 && len(n.idolNames) == 1 {
		customKey = fmt.Sprintf("v1/%s/%s/%s-%s-%s-%s%s", n.groupNames[0], n.idolNames[0], n.dateStr, n.groupNames[0], n.idolNames[0], shortId, ext)
	} else if len(n.groupNames) == 1 {
		customKey = fmt.Sprintf("v1/%s/%s-%s-%s-%s%s", n.groupNames[0], n.dateStr, n.groupsJoined, n.idolsJoined, shortId, ext)
	} else {
		customKey = fmt.Sprintf("v1/mix/%s-%s-%s-%s%s", n.dateStr, n.groupsJoined, n.idolsJoined, shortId, ext)
	}

	fs, err := app.NewFilesystem()
	if err != nil {
		LogUploadError(fmt.Sprintf("R2: filesystem init failed: %v", err), map[string]any{"record": recordId})
		return
	}
	defer fs.Close()

	// Every object this function puts in R2 is tracked so a failure anywhere
	// after the first upload — a later encode error, a failed upload, or the
	// final app.Save — doesn't leave orphans behind. Before this, an upload
	// followed by a failed save leaked silently, and there is no reprocess path
	// that would ever have overwritten those keys.
	var (
		uploaded  []string
		uploadMu  sync.Mutex
		committed bool
	)
	put := func(content []byte, key string) error {
		if err := fs.Upload(content, key); err != nil {
			return err
		}
		uploadMu.Lock()
		uploaded = append(uploaded, key)
		uploadMu.Unlock()
		return nil
	}
	defer func() {
		if committed || len(uploaded) == 0 {
			return
		}
		LogUploadWarning(
			fmt.Sprintf("R2: rolling back %d orphaned object(s) for record %s", len(uploaded), recordId),
			map[string]any{"record": recordId, "keys": uploaded},
		)
		enqueueR2DeleteKeys(uploaded...)
	}()

	pbKey := record.Collection().Id + "/" + record.Id + "/" + filename

	reader, err := fs.GetReader(pbKey)
	if err != nil {
		LogUploadError(fmt.Sprintf("R2: could not read %s: %v", pbKey, err), map[string]any{"record": recordId})
		return
	}
	content, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		LogUploadError(fmt.Sprintf("R2: could not read file content: %v", err), map[string]any{"record": recordId})
		return
	}

	r2BaseURL := strings.TrimRight(os.Getenv("R2_PUBLIC_URL"), "/")
	filetype := record.GetString("filetype")

	switch filetype {
	case "video":
		// The H.264 SD rendition encodes on the GPU's media engine while
		// libsvtav1 has the CPU cores to itself — separate silicon, so these
		// two genuinely overlap rather than compete (unlike two libsvtav1
		// instances, see the gif case below). SD is derived from the source,
		// not from the AV1 output, so it doesn't inherit AV1's artifacts.
		sd := startH264SD(content, ext, true, meta)

		transcoded, err := transcodeToAV1(content, ext, true, meta)
		// Join before any return so the GPU job can't outlive the record it
		// belongs to. Effectively free: a 720p GPU encode finishes long before
		// libsvtav1 does.
		sdBytes, sdErr := sd.wait()
		if err != nil {
			LogUploadError(fmt.Sprintf("R2: video transcode failed: %v", err), map[string]any{"record": recordId})
			fs.Delete(pbKey)
			return
		}
		keyBase := strings.TrimSuffix(customKey, ext)
		mp4Key := keyBase + ".mp4"
		if err := put(transcoded, mp4Key); err != nil {
			LogUploadError(fmt.Sprintf("R2: upload %s failed: %v", mp4Key, err), map[string]any{"record": recordId})
			return
		}
		fs.Delete(pbKey)
		mp4PublicURL := r2BaseURL + "/" + mp4Key
		record.Set("original", mp4PublicURL)
		record.Set("preview", mp4PublicURL)
		// The transcoded AV1, which is what `original` points at and what the
		// frontend renders.
		setRecordDimensions(record, transcoded, ".mp4", recordId)
		uploadSD(put, record, sdBytes, sdErr, r2BaseURL, keyBase)
		staticKey := keyBase + "-static.avif"
		if staticContent, err := generateStaticPreview(transcoded, ".mp4"); err != nil {
			log.Printf("⚠️  R2: video static preview failed: %v", err)
		} else if err := put(staticContent, staticKey); err != nil {
			log.Printf("⚠️  R2: static upload %s failed: %v", staticKey, err)
		} else {
			record.Set("static", r2BaseURL+"/"+staticKey)
		}
		notifyAvifReady(app, record, mp4PublicURL, n.dateStr)

	case "gif":
		// Serial encode pipeline (everything CPU-bound now that QSV/AVIF is
		// off the table due to ffmpeg's avif muxer not handling hardware AV1
		// streams — see libavif issue #2922):
		//   1. MP4+AV1 from source (libsvtav1 c34-p6-tuned-10b, ~5s)
		//   2. Animated preview (AVIF or WebP) from the encoded MP4 bytes
		//      (~2-3s, smaller input than source → faster decode, no
		//      double-scaling). Format picked by the `preview_format` field.
		//   3. Static thumb (same codec as the preview) from the encoded MP4
		//      bytes (~0.5-1s, single frame)
		// Parallel encoding here would just have two libsvtav1 instances
		// fighting for the same 12 threads — no real speedup, just CPU
		// contention. Serial keeps each encode at full throughput.
		//
		// Uploads to R2 run in parallel since they're network-bound.
		//
		// The one thing that *is* parallel on the encode side is the H.264 SD
		// rendition, which runs on the GPU's media engine and so overlaps the
		// CPU chain above for free. When it starts depends on interpolation:
		// without it, SD derives from the source and begins immediately;
		// with it, SD has to derive from the interpolated MP4 or the fallback
		// would show different frames than the canonical rendition, so it
		// starts after RIFE and overlaps the preview encodes instead.
		interpolateFactor := record.GetFloat("interpolate")
		interpolating := interpolateFactor > 1.0

		var sd *h264Job
		if !interpolating {
			sd = startH264SD(content, ext, false, meta)
		}

		mp4Bytes, err := transcodeGifToAV1Tuned(content, ext, meta)
		if err != nil {
			sd.wait() // don't leave the GPU job orphaned behind the return
			LogUploadError(fmt.Sprintf("R2: gif transcode failed: %v", err), map[string]any{"record": recordId})
			fs.Delete(pbKey)
			return
		}

		// Optional RIFE interpolation step (GPU, Vulkan).
		// `interpolate`      = factor (1.0 = no-op, 1.25, 1.5, or 2.0)
		// `interpolate_mode` = "slow" (default, longer duration) or "smooth"
		//                     (same duration, higher fps — for already-slowed
		//                     source content)
		// When factor >1.0, mp4Bytes is replaced with the interpolated version,
		// which then feeds both the AVIF and static thumbnail generators below
		// — so the final preview matches the final MP4.
		if interpolating {
			mode := record.GetString("interpolate_mode")
			if mode == "" {
				mode = "slow"
			}
			log.Printf("🎞️  R2: interpolating gif %.2fx (%s mode, %d KB pre)",
				interpolateFactor, mode, len(mp4Bytes)/1024)
			if interpolated, ierr := InterpolateMP4(mp4Bytes, interpolateFactor, mode); ierr != nil {
				log.Printf("⚠️  R2: interpolation failed, using non-interpolated: %v", ierr)
			} else {
				log.Printf("✅ R2: interpolated %.2fx %s (%d KB post)",
					interpolateFactor, mode, len(interpolated)/1024)
				mp4Bytes = interpolated
			}

			// Started here, not earlier, for correctness: SD has to encode
			// the same frames the canonical MP4 ends up with, and that isn't
			// known until interpolation has run (or failed and left mp4Bytes
			// alone). Scheduling is a happy side effect — RIFE occupies the
			// Arc's shader cores while VAAPI wants its fixed-function
			// encoder, so this also keeps the two off each other's VRAM.
			sd = startH264SD(mp4Bytes, ".mp4", false, meta)
		}

		// Preview format is per-upload. The `preview_format` record field
		// (set by the FE) picks the animated preview + static poster codec.
		// Anything but "webp" — empty, "avif", junk — maps to AVIF, the
		// historical default, so every non-FE caller and every existing record
		// keeps its exact prior behavior. "webp" swaps in animated WebP + a
		// WebP poster for broader device support. Both the preview and the
		// poster use the same codec so a WebP record is fully AVIF-free (an
		// AVIF-incapable device can't decode an AVIF poster either).
		useWebP := record.GetString("preview_format") == "webp"
		previewFmt := "avif"
		animExt, staticSuffix := ".avif", "-static.avif"
		if useWebP {
			previewFmt = "webp"
			animExt, staticSuffix = ".webp", "-static.webp"
		}

		// Re-use mp4Bytes as the preview/static source. Generational loss at
		// preview size is imperceptible; the preview encode runs faster on
		// the smaller pre-scaled input.
		var (
			animBytes, staticBytes []byte
			animErr, staticErr     error
		)
		if useWebP {
			animBytes, animErr = generateAnimatedWebP(mp4Bytes, ".mp4")
			staticBytes, staticErr = generateStaticWebP(mp4Bytes, ".mp4")
		} else {
			animBytes, animErr = generateAnimatedAVIF(mp4Bytes, ".mp4")
			staticBytes, staticErr = generateStaticPreview(mp4Bytes, ".mp4")
		}
		if animErr != nil {
			log.Printf("⚠️  R2: animated %s encode failed: %v", previewFmt, animErr)
		}
		if staticErr != nil {
			log.Printf("⚠️  R2: gif static preview failed: %v", staticErr)
		}

		// Join the GPU job before uploading. By now the CPU has finished the
		// AV1 encode plus both preview encodes, so a 720p GPU job has
		// almost certainly been done for a while.
		sdBytes, sdErr := sd.wait()

		keyBase := strings.TrimSuffix(customKey, ext)
		mp4Key := keyBase + ".mp4"
		animKey := keyBase + animExt
		staticKey := keyBase + staticSuffix
		sdKey := keyBase + "-sd.mp4"

		// Parallel uploads (MP4 fatal-on-failure, preview/static/sd best-effort).
		var (
			mp4UpErr, animUpErr, staticUpErr, sdUpErr error
			upWG                                      sync.WaitGroup
		)
		upWG.Add(1)
		go func() {
			defer upWG.Done()
			mp4UpErr = put(mp4Bytes, mp4Key)
		}()
		if animErr == nil {
			upWG.Add(1)
			go func() {
				defer upWG.Done()
				animUpErr = put(animBytes, animKey)
			}()
		}
		if staticErr == nil {
			upWG.Add(1)
			go func() {
				defer upWG.Done()
				staticUpErr = put(staticBytes, staticKey)
			}()
		}
		if sdErr == nil && len(sdBytes) > 0 {
			upWG.Add(1)
			go func() {
				defer upWG.Done()
				sdUpErr = put(sdBytes, sdKey)
			}()
		}
		upWG.Wait()

		if mp4UpErr != nil {
			LogUploadError(fmt.Sprintf("R2: upload %s failed: %v", mp4Key, mp4UpErr), map[string]any{"record": recordId})
			return
		}
		fs.Delete(pbKey)
		mp4PublicURL := r2BaseURL + "/" + mp4Key
		record.Set("original", mp4PublicURL)
		// mp4Bytes, so an interpolated gif is measured AFTER RIFE — the preview
		// and static are generated from these same bytes, so this matches every
		// rendition the frontend can render.
		setRecordDimensions(record, mp4Bytes, ".mp4", recordId)

		// Animated preview: success → set preview; failure (encode or upload) → fall back to MP4 URL.
		if animErr != nil || animUpErr != nil {
			if animUpErr != nil {
				log.Printf("⚠️  R2: %s upload failed: %v", previewFmt, animUpErr)
			}
			record.Set("preview", mp4PublicURL)
		} else {
			animPublicURL := r2BaseURL + "/" + animKey
			record.Set("preview", animPublicURL)
			notifyAvifReady(app, record, animPublicURL, n.dateStr)
		}

		// Static thumbnail: best-effort.
		if staticErr == nil && staticUpErr == nil {
			record.Set("static", r2BaseURL+"/"+staticKey)
		} else if staticUpErr != nil {
			log.Printf("⚠️  R2: static upload %s failed: %v", staticKey, staticUpErr)
		}

		// H.264 fallback: best-effort. A record without it still plays
		// everywhere the AV1 does — it just loses the older devices.
		if sdErr != nil {
			log.Printf("⚠️  R2: SD (H.264) encode failed: %v", sdErr)
		} else if sdUpErr != nil {
			log.Printf("⚠️  R2: SD upload %s failed: %v", sdKey, sdUpErr)
		} else if len(sdBytes) > 0 {
			record.Set("sd", r2BaseURL+"/"+sdKey)
		}

	case "sticker":
		avifContent, err := generateStickerAVIF(content, ext)
		if err != nil {
			LogUploadError(fmt.Sprintf("R2: sticker→AVIF failed: %v", err), map[string]any{"record": recordId})
			fs.Delete(pbKey)
			return
		}
		avifKey := strings.TrimSuffix(customKey, ext) + ".avif"
		if err := put(avifContent, avifKey); err != nil {
			LogUploadError(fmt.Sprintf("R2: upload %s failed: %v", avifKey, err), map[string]any{"record": recordId})
			return
		}
		fs.Delete(pbKey)
		avifPublicURL := r2BaseURL + "/" + avifKey
		record.Set("original", avifPublicURL)
		record.Set("preview", avifPublicURL)
		// Fixed, not probed: generateStickerAVIF crops to a square and scales to
		// 200×200 whatever went in, and probing its output would mean relying on
		// ffmpeg decoding an avifenc-produced AVIF.
		record.Set("width", stickerDimensions)
		record.Set("height", stickerDimensions)

	case "image":
		keyBase := strings.TrimSuffix(customKey, ext)

		// The SOURCE, not the AVIF renditions: stillScaleCap caps the longer edge
		// without cropping, so the ratio is identical, and this avoids depending
		// on ffmpeg's AVIF decode support. Probed before the encodes so a still
		// that fails to encode doesn't matter either way.
		setRecordDimensions(record, content, ext, recordId)

		// `original` and `preview` are DISTINCT objects for stills. They used to
		// be the same key, which was fine while the canonical still was capped
		// at ~1080p — but the frontend loads `original` for image cards, so
		// raising the cap to 2K without splitting them would have made the
		// masonry grid pull multi-megabyte files per tile.
		fullContent, err := encodeStillAVIF(content, ext)
		if err != nil {
			LogUploadError(fmt.Sprintf("R2: image→AVIF failed: %v", err), map[string]any{"record": recordId})
			fs.Delete(pbKey)
			return
		}
		fullKey := keyBase + ".avif"
		if err := put(fullContent, fullKey); err != nil {
			LogUploadError(fmt.Sprintf("R2: upload %s failed: %v", fullKey, err), map[string]any{"record": recordId})
			return
		}
		record.Set("original", r2BaseURL+"/"+fullKey)

		// Grid rendition. Best-effort: falling back to the full-size object
		// costs bandwidth, whereas failing the record loses the upload.
		previewURL := r2BaseURL + "/" + fullKey
		previewKey := keyBase + "-preview.avif"
		if previewContent, err := encodeStillPreviewAVIF(content, ext); err != nil {
			log.Printf("⚠️  R2: image preview encode failed, pointing preview at the full still: %v", err)
		} else if err := put(previewContent, previewKey); err != nil {
			log.Printf("⚠️  R2: preview upload %s failed: %v", previewKey, err)
		} else {
			previewURL = r2BaseURL + "/" + previewKey
		}
		record.Set("preview", previewURL)

		staticKey := keyBase + "-static.avif"
		if staticContent, err := generateStaticPreview(content, ext); err != nil {
			log.Printf("⚠️  R2: image static preview failed: %v", err)
		} else if err := put(staticContent, staticKey); err != nil {
			log.Printf("⚠️  R2: static upload %s failed: %v", staticKey, err)
		} else {
			record.Set("static", r2BaseURL+"/"+staticKey)
		}
		fs.Delete(pbKey)
		notifyAvifReady(app, record, previewURL, n.dateStr)

	default:
		// Previously this was the "image" branch, so an upload with an empty or
		// unrecognised filetype was silently reduced to a single AVIF frame and
		// its source destroyed. contents.createRule only requires canUpload, so
		// that was reachable by any API caller, not just our own frontend.
		//
		// pbKey is deliberately left in place: without a filetype we don't know
		// what to encode, and keeping the upload means the record can be fixed
		// and reprocessed instead of losing the file.
		LogUploadError(
			fmt.Sprintf("R2: record %s has unsupported filetype %q — leaving the upload untouched", recordId, filetype),
			map[string]any{"record": recordId, "filetype": filetype},
		)
		return
	}

	record.Set("file", nil)

	// Save onto a FRESH copy of the row, not the one loaded when the encode
	// started. Encodes run for seconds to minutes, and an edit made in that
	// window (a title fix in the Details view, a set change) lives only in the
	// database — writing the whole stale record back would silently revert it.
	// The pipeline owns exactly the fields copied below; everything else on the
	// row belongs to whoever touched it in the meantime.
	fresh, err := app.FindRecordById("contents", recordId)
	if err != nil {
		// Deleted mid-encode (a set cascade, a moderator). committed stays
		// false, so the deferred rollback reclaims every object uploaded above.
		LogUploadError(fmt.Sprintf("R2: record %s vanished during processing: %v", recordId, err),
			map[string]any{"record": recordId})
		return
	}
	for _, field := range []string{"original", "preview", "static", "sd", "width", "height", "file"} {
		fresh.Set(field, record.Get(field))
	}

	// SaveNoValidate: every field written above is computed by this pipeline, and
	// validating the whole record here means an unrelated problem elsewhere on it
	// discards finished work. That happened — a set deleted while its child was
	// still encoding left a dangling `set` relation, so this save failed with
	// "Failed to find all relation records with the provided ids", the deferred
	// rollback deleted four freshly-uploaded R2 objects, and the record was left
	// with no URLs at all despite the encode having succeeded.
	if err := app.SaveNoValidate(fresh); err != nil {
		LogUploadError(fmt.Sprintf("R2: could not update record %s: %v", recordId, err), map[string]any{"record": recordId})
	} else {
		// Past this point the record owns the objects, so the rollback deferred
		// above must not fire.
		committed = true
		log.Printf("✅ R2: processed %s → %s", recordId, record.GetString("original"))
		// Only after a successful save — the announcement reads the record back
		// out of the database and needs the URL fields committed.
		if OnContentPublished != nil {
			go OnContentPublished(recordId)
		}
	}
}

// resolveDisplayName returns the raw "name" field of the first relation record.
func resolveDisplayName(app *pocketbase.PocketBase, record *core.Record, field, collectionId string) string {
	ids := record.GetStringSlice(field)
	if len(ids) == 0 {
		return ""
	}
	rel, err := app.FindRecordById(collectionId, ids[0])
	if err != nil {
		return ""
	}
	return rel.GetString("name")
}

func resolveAllRelationNames(app *pocketbase.PocketBase, ids []string, collectionId string) []string {
	var names []string
	for _, id := range ids {
		rel, err := app.FindRecordById(collectionId, id)
		if err != nil {
			names = append(names, Slugify(id))
			continue
		}
		name := rel.GetString("name")
		if name == "" {
			name = id
		}
		names = append(names, Slugify(name))
	}
	return names
}

func recordDateString(record *core.Record) string {
	for _, field := range []string{"date", "created"} {
		val := record.GetString(field)
		if val == "" {
			continue
		}
		for _, layout := range []string{
			"2006-01-02 15:04:05.000Z",
			"2006-01-02 15:04:05.999Z",
			"2006-01-02",
		} {
			if t, err := time.Parse(layout, val); err == nil {
				return t.Format("060102")
			}
		}
	}
	return time.Now().UTC().Format("060102")
}

// Slugify lowercases, collapses spaces to hyphens and strips everything that
// isn't [a-z0-9-]. Exported because the bot needs it to derive `tags.code`, and
// because label slugs must be computed the same way everywhere.
//
// Returns "" for input with no alphanumerics at all ("!!!"), so callers that
// feed it into a required field have to handle the empty case.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.Trim(b.String(), "-")
}
