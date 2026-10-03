package hooks

import (
	"errors"
	"fmt"
	"time"
)

// ConvertKind selects which encode pipeline Convert runs.
type ConvertKind string

const (
	ConvertAVIF    ConvertKind = "avif"    // animated AVIF preview (same as upload pipeline)
	ConvertWebP    ConvertKind = "webp"    // animated WebP preview (AVIF alternative, wider device support)
	ConvertMP4     ConvertKind = "mp4"     // MP4+AV1 transcode, audio kept
	ConvertSD      ConvertKind = "sd"      // MP4+H.264 720p fallback, audio kept (GPU)
	ConvertSticker ConvertKind = "sticker" // 200×200 square animated AVIF
	ConvertThumb   ConvertKind = "thumb"   // static first-frame AVIF thumbnail
)

// ErrBusy is returned when no processing slot frees up within the wait window.
var ErrBusy = errors.New("encode queue is busy, try again shortly")

// Convert runs one of the encode pipelines on in-memory media and returns the
// output bytes plus the output file extension. Callers outside the upload
// pipeline (the /api/convert route, the Discord bot) must go through this so
// every ffmpeg job is gated by the shared process semaphore.
func Convert(kind ConvertKind, content []byte, srcExt string) ([]byte, string, error) {
	// Counted in the same queue uploads report, because it competes for the same
	// semaphore: an uploader watching the depth should see a bot reupload or an
	// /api/convert call occupying the encoder, not a queue of zero while nothing
	// of theirs moves.
	releaseQueued := enqueueJob("convert")
	defer releaseQueued()

	select {
	case processSem <- struct{}{}:
		releaseActive := beginActive()
		defer func() {
			releaseActive()
			<-processSem
		}()
	case <-time.After(30 * time.Second):
		return nil, "", ErrBusy
	}

	switch kind {
	case ConvertAVIF:
		out, err := generateAnimatedAVIF(content, srcExt)
		return out, ".avif", err
	case ConvertWebP:
		out, err := generateAnimatedWebP(content, srcExt)
		return out, ".webp", err
	case ConvertMP4:
		out, err := transcodeToAV1(content, srcExt, true, nil)
		return out, ".mp4", err
	case ConvertSD:
		out, err := transcodeToH264SD(content, srcExt, true, nil)
		return out, ".mp4", err
	case ConvertSticker:
		out, err := generateStickerAVIF(content, srcExt)
		return out, ".avif", err
	case ConvertThumb:
		out, err := generateStaticPreview(content, srcExt)
		return out, ".avif", err
	}
	return nil, "", fmt.Errorf("unknown convert kind %q", kind)
}
