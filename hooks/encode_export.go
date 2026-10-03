package hooks

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// The encode pipeline, callable from outside the server process.
//
// moveFileToCustomR2Path is the pipeline as the server runs it: it reads the
// upload out of PocketBase storage, encodes, uploads to R2 and writes the URLs
// back. scripts/backfilldiscord runs the same encodes on a different machine and
// hands the server finished renditions, so the encode step has to be reachable
// without a record, a filesystem or an app. That is what this file exports.
//
// Everything here delegates to the unexported encoders the server uses, so a
// rendition produced by the script is byte-for-byte what the server would have
// produced from the same source at the same commit. Keep it that way: if a
// branch in moveFileToCustomR2Path changes, the matching branch in
// EncodeRenditions changes in the same PR. The interpolation step is the one
// deliberate omission — it needs RIFE, which the backfill machine doesn't have,
// and Discord ingests never ask for it.

// Rendition is one encoded object: the bytes and the suffix it is stored under,
// appended to the record's key base (see ContentKeyBase).
type Rendition struct {
	Suffix string
	Bytes  []byte
}

// Renditions is the full output for one upload. Original is always present;
// the others are best-effort, exactly as in the server pipeline, and nil when
// their encode failed. A nil Preview means the preview URL should point at the
// original, which is what the server does for videos and for a failed animated
// preview.
type Renditions struct {
	Original Rendition
	Preview  *Rendition
	Static   *Rendition
	SD       *Rendition
	Width    int
	Height   int
}

// EncodeRenditions runs the encode branch for `filetype` over `content`.
//
// `ext` is the source file's extension including the dot — ffmpeg picks its
// demuxer from it. `previewFormat` is the record's preview_format ("webp" or
// anything else for AVIF). `tags` is the provenance tag set (RenditionTags),
// stamped into every MP4 rendition.
func EncodeRenditions(content []byte, ext, filetype, previewFormat string, tags map[string]string) (*Renditions, error) {
	meta := metadataArgs(tags)
	out := &Renditions{}

	switch filetype {
	case "video":
		sd := startH264SD(content, ext, true, meta)
		transcoded, err := transcodeToAV1(content, ext, true, meta)
		sdBytes, sdErr := sd.wait()
		if err != nil {
			return nil, fmt.Errorf("video transcode: %w", err)
		}
		out.Original = Rendition{Suffix: ".mp4", Bytes: transcoded}
		out.Width, out.Height = dimensionsOrZero(transcoded, ".mp4")
		out.SD = sdRendition(sdBytes, sdErr)
		if static, err := generateStaticPreview(transcoded, ".mp4"); err != nil {
			log.Printf("⚠️  encode: video static preview failed: %v", err)
		} else {
			out.Static = &Rendition{Suffix: "-static.avif", Bytes: static}
		}

	case "gif":
		sd := startH264SD(content, ext, false, meta)
		mp4Bytes, err := transcodeGifToAV1Tuned(content, ext, meta)
		if err != nil {
			sd.wait()
			return nil, fmt.Errorf("gif transcode: %w", err)
		}
		out.Original = Rendition{Suffix: ".mp4", Bytes: mp4Bytes}
		out.Width, out.Height = dimensionsOrZero(mp4Bytes, ".mp4")

		useWebP := previewFormat == "webp"
		var (
			animBytes, staticBytes []byte
			animErr, staticErr     error
			animExt, staticSuffix  = ".avif", "-static.avif"
		)
		if useWebP {
			animExt, staticSuffix = ".webp", "-static.webp"
			animBytes, animErr = generateAnimatedWebP(mp4Bytes, ".mp4")
			staticBytes, staticErr = generateStaticWebP(mp4Bytes, ".mp4")
		} else {
			animBytes, animErr = generateAnimatedAVIF(mp4Bytes, ".mp4")
			staticBytes, staticErr = generateStaticPreview(mp4Bytes, ".mp4")
		}
		if animErr != nil {
			log.Printf("⚠️  encode: animated %s failed: %v", strings.TrimPrefix(animExt, "."), animErr)
		} else {
			out.Preview = &Rendition{Suffix: animExt, Bytes: animBytes}
		}
		if staticErr != nil {
			log.Printf("⚠️  encode: gif static preview failed: %v", staticErr)
		} else {
			out.Static = &Rendition{Suffix: staticSuffix, Bytes: staticBytes}
		}
		out.SD = sdRendition(sd.wait())

	case "sticker":
		avif, err := generateStickerAVIF(content, ext)
		if err != nil {
			return nil, fmt.Errorf("sticker encode: %w", err)
		}
		out.Original = Rendition{Suffix: ".avif", Bytes: avif}
		out.Width, out.Height = stickerDimensions, stickerDimensions

	case "image":
		out.Width, out.Height = dimensionsOrZero(content, ext)
		full, err := encodeStillAVIF(content, ext)
		if err != nil {
			return nil, fmt.Errorf("still encode: %w", err)
		}
		out.Original = Rendition{Suffix: ".avif", Bytes: full}
		if preview, err := encodeStillPreviewAVIF(content, ext); err != nil {
			log.Printf("⚠️  encode: still preview failed, preview will point at the full still: %v", err)
		} else {
			out.Preview = &Rendition{Suffix: "-preview.avif", Bytes: preview}
		}
		if static, err := generateStaticPreview(content, ext); err != nil {
			log.Printf("⚠️  encode: still static preview failed: %v", err)
		} else {
			out.Static = &Rendition{Suffix: "-static.avif", Bytes: static}
		}

	default:
		return nil, fmt.Errorf("unsupported filetype %q", filetype)
	}

	return out, nil
}

func sdRendition(sdBytes []byte, sdErr error) *Rendition {
	if sdErr != nil {
		log.Printf("⚠️  encode: SD (H.264) failed: %v", sdErr)
		return nil
	}
	if len(sdBytes) == 0 {
		return nil
	}
	return &Rendition{Suffix: "-sd.mp4", Bytes: sdBytes}
}

// dimensionsOrZero mirrors setRecordDimensions: a failed probe leaves 0/0, which
// the frontend reads as "unknown" and measures on load.
func dimensionsOrZero(content []byte, ext string) (int, int) {
	w, h, err := probeDimensions(content, ext)
	if err != nil {
		log.Printf("⚠️  encode: could not probe dimensions: %v", err)
		return 0, 0
	}
	return w, h
}

// ContentKeyBase is the R2 object key for a record, minus the extension — the
// same derivation moveFileToCustomR2Path makes, so the script's objects land
// where a server encode of the same record would have put them and the update
// cleanup hook's "same metadata, same keys" assumption keeps holding.
//
// groupSlugs and idolSlugs are the record's relation names already through
// Slugify, in relation order (resolveAllRelationNames). dateStr is YYMMDD.
func ContentKeyBase(filetype string, groupSlugs, idolSlugs []string, dateStr, recordID string) string {
	groupsJoined := strings.Join(groupSlugs, "-")
	idolsJoined := strings.Join(idolSlugs, "-")
	if groupsJoined == "" {
		groupsJoined = "unknown"
	}
	if idolsJoined == "" {
		idolsJoined = "unknown"
	}
	shortId := recordID
	if len(shortId) > 4 {
		shortId = shortId[len(shortId)-4:]
	}
	switch {
	case filetype == "sticker":
		return fmt.Sprintf("v1/stickers/%s-%s-%s-%s", dateStr, groupsJoined, idolsJoined, shortId)
	case len(groupSlugs) == 1 && len(idolSlugs) == 1:
		return fmt.Sprintf("v1/%s/%s/%s-%s-%s-%s", groupSlugs[0], idolSlugs[0], dateStr, groupSlugs[0], idolSlugs[0], shortId)
	case len(groupSlugs) == 1:
		return fmt.Sprintf("v1/%s/%s-%s-%s-%s", groupSlugs[0], dateStr, groupsJoined, idolsJoined, shortId)
	default:
		return fmt.Sprintf("v1/mix/%s-%s-%s-%s", dateStr, groupsJoined, idolsJoined, shortId)
	}
}

// RenditionTags is renditionTags for a caller that has the names in hand rather
// than a record to resolve them from. Same keys, same formats; see filemeta.go
// for why each exists.
func RenditionTags(recordID, title, filename string, uploaders, idols, groups []string, created, date time.Time) map[string]string {
	uploader := strings.Join(uploaders, ", ")
	tags := map[string]string{
		"title":   title,
		"artist":  uploader,
		"comment": siteBaseURL() + "/single/" + recordID,

		"goyangi_id":       recordID,
		"goyangi_uploader": uploader,
		"goyangi_filename": filename,
		"goyangi_idol":     strings.Join(idols, ", "),
		"goyangi_group":    strings.Join(groups, ", "),
		"goyangi_uploaded": created.UTC().Format("2006-01-02"),
	}
	if !date.IsZero() {
		iso := date.UTC().Format("2006-01-02")
		tags["date"] = iso
		tags["goyangi_date"] = iso
	}
	return tags
}

// ClassifyBytes is the byte-level still-or-animation decision the create hook
// applies to uploads (reclassifyContentType), for content that never becomes an
// upload. Only meaningful for an `image` or `gif` hint; the caller keeps the
// hint on error, as the hook does.
func ClassifyBytes(content []byte, ext string) (string, error) {
	tmp, err := os.CreateTemp("", "goyangi-classify-*"+ext)
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
