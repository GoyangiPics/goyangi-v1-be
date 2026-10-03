package hooks

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
)

// Whether an upload is a still or an animation is decided from its bytes, not
// from the tab it was dropped on.
//
// The upload page used to send `filetype` straight from the selected tab, and
// the encode hook trusted it verbatim. A .webm dropped while "Pics" was active
// went through the still pipeline and came out as a one-frame AVIF. The page
// now sends a hint (video container → gif, image mime → image) and this hook
// corrects it: for the ambiguous formats — WebP and AVIF can both be either —
// the extension and the mime type cannot know, and a frame count can.
//
// Only `image` and `gif` are reclassified. `video` is a semantic choice (keep
// the audio, don't autoplay) that no container can make, and `sticker` is a
// pipeline, not a description of the file.

// reclassifyContentType runs in the create hook, BEFORE the record is saved,
// so everything downstream — the queue bucket, the encode branch — sees the
// corrected value and nothing has to be fixed up later.
//
// Fails OPEN: a probe error keeps the client's hint. Losing an upload to a
// classification hiccup is the worse outcome, and the hint is right for every
// unambiguous container anyway.
func reclassifyContentType(record *core.Record) {
	hinted := record.GetString("filetype")
	if hinted != "image" && hinted != "gif" {
		return
	}
	files := record.GetUnsavedFiles("file")
	if len(files) != 1 {
		return
	}
	detected, err := classifyStillOrAnimated(files[0])
	if err != nil {
		LogUploadWarning(fmt.Sprintf("classify: could not probe %q, keeping hint %q: %v", files[0].Name, hinted, err),
			map[string]any{"filename": files[0].Name, "hint": hinted})
		return
	}
	if detected != hinted {
		LogUploadWarning(fmt.Sprintf("classify: %q was hinted %q, bytes say %q", files[0].Name, hinted, detected),
			map[string]any{"filename": files[0].Name, "hint": hinted, "detected": detected})
		record.Set("filetype", detected)
	}
}

// classifyStillOrAnimated probes an unsaved upload's frame count.
//
// Through a temp file: the upload is already fully in memory at this point
// (PocketBase has parsed the multipart body), so the copy costs a write, and
// ffprobe wants a path. The extension is kept so the demuxer is picked the same
// way the encode pipeline will pick it.
func classifyStillOrAnimated(f *filesystem.File) (string, error) {
	r, err := f.Reader.Open()
	if err != nil {
		return "", fmt.Errorf("open upload: %w", err)
	}
	defer r.Close()
	content, err := io.ReadAll(r)
	if err != nil {
		return "", fmt.Errorf("read upload: %w", err)
	}

	tmp, err := os.CreateTemp("", "goyangi-classify-*"+filepath.Ext(f.Name))
	if err != nil {
		return "", fmt.Errorf("temp file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		return "", fmt.Errorf("write temp: %w", err)
	}
	tmp.Close()

	frames, err := probeVideoFrameCount(tmp.Name())
	if err != nil {
		return "", err
	}
	return filetypeForFrameCount(frames), nil
}

// probeVideoFrameCount counts the packets in the first video stream.
//
// Packets rather than `nb_frames`: that field is absent from containers that
// don't store it (WebM, animated WebP), and a packet count is a demux-only pass
// that every format supports. A still image has exactly one.
func probeVideoFrameCount(path string) (int, error) {
	out, err := runProbe("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-count_packets",
		"-show_entries", "stream=nb_read_packets",
		"-of", "csv=p=0",
		path,
	)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("unexpected probe output %q: %w", out, err)
	}
	return n, nil
}

// filetypeForFrameCount is the whole decision, kept pure so it can be tested
// without ffprobe: more than one frame is an animation.
func filetypeForFrameCount(frames int) string {
	if frames > 1 {
		return "gif"
	}
	return "image"
}
