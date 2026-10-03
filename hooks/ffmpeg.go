package hooks

import (
	"fmt"
	"os"
)

// scaleDown1080p caps the video's LONGER edge at 1920 whatever the orientation,
// never upscaling. Landscape comes out ≤1920×1080 exactly as before; portrait
// now comes out ≤1080×1920.
//
// The box is square for the reason stillScaleCap's is (that fix came first —
// see its comment): the old asymmetric 1920×1080 box read min() of the SOURCE
// on both axes, so a portrait 1080×1920 was boxed at 1080×1080 and encoded at
// ~608×1080 — a landscape cap silently applied to portrait content.
//
// force_divisible_by=2 also fixes a failure the old box could produce on its
// own: yuv420p10le needs even dimensions and libsvtav1 errors on odd ones, and
// force_original_aspect_ratio=decrease is free to land on an odd edge.
const scaleDown1080p = "scale='min(iw,1920)':'min(ih,1920)':force_original_aspect_ratio=decrease:force_divisible_by=2,setsar=1"

// runFFmpeg writes content to a temp input file, runs ffmpeg with the given
// args spliced between the input and output flags, and returns the bytes of
// the outExt-typed output file.
func runFFmpeg(content []byte, srcExt, outExt string, args ...string) ([]byte, error) {
	return runFFmpegWith(content, srcExt, outExt, nil, args)
}

// runFFmpegWith is runFFmpeg with an extra slot for ffmpeg *global* options,
// which must precede -i on the command line. Only the hardware paths need it
// (-vaapi_device); everything else goes through runFFmpeg.
func runFFmpegWith(content []byte, srcExt, outExt string, preInput, args []string) ([]byte, error) {
	input, err := os.CreateTemp("", "goyangi-in-*"+srcExt)
	if err != nil {
		return nil, fmt.Errorf("create temp input: %w", err)
	}
	defer os.Remove(input.Name())
	if _, err := input.Write(content); err != nil {
		input.Close()
		return nil, fmt.Errorf("write temp input: %w", err)
	}
	input.Close()

	output, err := os.CreateTemp("", "goyangi-out-*"+outExt)
	if err != nil {
		return nil, fmt.Errorf("create temp output: %w", err)
	}
	output.Close()
	defer os.Remove(output.Name())

	full := append([]string{}, preInput...)
	full = append(full, "-i", input.Name())
	full = append(full, args...)
	full = append(full, "-y", output.Name())
	if err := runEncoder("ffmpeg", full...); err != nil {
		return nil, err
	}

	return os.ReadFile(output.Name())
}

// transcodeToAV1 converts any video to MP4+AV1, returns .mp4 bytes.
// Pass keepAudio=true for videos that should retain their audio track.
//
// Used by the "video" filetype path (which needs audio preserved). For "gif"
// filetype use transcodeGifToAV1Tuned instead — same codec and bit depth but
// tuned params, picked by empirical bake-off as size:quality winner.
//
// `meta` is the -metadata argument list from metadataArgs (nil for none).
func transcodeToAV1(content []byte, srcExt string, keepAudio bool, meta []string) ([]byte, error) {
	args := []string{
		"-vf", scaleDown1080p,
		"-c:v", "libsvtav1",
		"-crf", "35",
		"-preset", "6",
		"-pix_fmt", "yuv420p10le",
	}
	if keepAudio {
		args = append(args, "-c:a", "aac")
	} else {
		args = append(args, "-an")
	}
	args = append(args, meta...)
	// +faststart moves the moov atom to the front. Without it a player has to
	// fetch the whole file before it can render the first frame.
	// use_metadata_tags keeps the goyangi_* keys — see filemeta.go.
	args = append(args, "-movflags", "+faststart+use_metadata_tags")
	return runFFmpeg(content, srcExt, ".mp4", args...)
}

// transcodeGifToAV1Tuned encodes a "gif" upload to MP4+AV1, long edge capped at
// 1920, 10-bit, with SVT-AV1 tuning params. Result of empirical bake-off across
// ~30 sources and ~70 variants — best size:quality at the 15s/file encode
// budget.
//
// Settings:
//   - libsvtav1 (CPU)
//   - crf 34 / preset 6
//   - yuv420p10le (10-bit — AV1's design home, slight quality lift)
//   - tune=0:aq-mode=2:enable-tf=1:film-grain=0:lookahead=60
//   - long edge ≤1920, source fps preserved, audio stripped
//
// Typical encode time on i5-10400F: ~5s avg, ~12s worst-case on 13s portrait —
// measured BEFORE the square box, when portrait encoded at ~608×1080. Portrait
// now carries ~3× the pixels (1080×1920), so expect its encodes near ~3× that.
func transcodeGifToAV1Tuned(content []byte, srcExt string, meta []string) ([]byte, error) {
	args := []string{
		"-vf", scaleDown1080p,
		"-c:v", "libsvtav1",
		"-crf", "34",
		"-preset", "6",
		"-pix_fmt", "yuv420p10le",
		"-svtav1-params", "tune=0:aq-mode=2:enable-tf=1:film-grain=0:lookahead=60",
		"-an",
	}
	args = append(args, meta...)
	args = append(args, "-movflags", "+faststart+use_metadata_tags")
	return runFFmpeg(content, srcExt, ".mp4", args...)
}

// generateStickerAVIF creates an animated AVIF from video bytes, square-cropped
// to 200×200.
func generateStickerAVIF(content []byte, srcExt string) ([]byte, error) {
	const squareCrop = "crop='min(iw,ih)':'min(iw,ih)',scale=200:200"
	const svtav1Params = "tune=0:aq-mode=2:enable-tf=1:film-grain=0:enable-restoration=1:enable-cdef=1:fast-decode=1:tile-columns=1:tile-rows=1:lookahead=60"

	return runFFmpeg(content, srcExt, ".avif",
		"-c:v", "libsvtav1",
		"-crf", "45",
		"-preset", "4",
		"-pix_fmt", "yuv420p10le",
		"-vf", squareCrop,
		"-svtav1-params", svtav1Params,
		"-an",
	)
}

// Long-edge caps for the two still renditions.
//
// stillFullPx is what `original` is capped at, and 2560 rather than 1920
// because a still is the one thing a viewer opens expecting to inspect it.
// stillPreviewPx feeds the masonry grid, where a dozen tiles load at once, so
// it stays small.
const (
	stillFullPx    = 2560
	stillPreviewPx = 1280
)

// stillScaleCap builds a scale filter that caps the LONGER edge at px whatever
// the orientation, never upscales, and keeps both dimensions even.
//
// The square box is the whole trick. scaleDown1080p used to be an asymmetric
// 1920×1080 box whose min() terms both read the *source*, so a portrait
// 1080×1920 was boxed at 1080×1080 and came out ~608×1080 — a landscape cap
// silently applied to portrait content, which is what "pics look low quality"
// actually was. With a square box, force_original_aspect_ratio=decrease fits
// the source inside it, the long edge lands on px and the short edge follows
// the aspect ratio. The AV1 box (scaleDown1080p) is square now too; the SD box
// stays asymmetric on purpose — see scaleDown720p.
//
// force_divisible_by=2 is required for yuv420p (libsvtav1 errors on odd
// dimensions); it rounds down, so 1081×1921 → 1080×1920.
//
// out_range=pc performs the conversion that -color_range pc only *declares*.
// Tagging limited-range pixels as full range shifts colours; doing both makes
// the tag truthful.
func stillScaleCap(px int) string {
	return fmt.Sprintf(
		"scale='min(iw,%d)':'min(ih,%d)':force_original_aspect_ratio=decrease:force_divisible_by=2:out_range=pc,setsar=1",
		px, px,
	)
}

// encodeStillAVIF is the canonical still: 2K long edge, quality first.
//
// crf 20 (was 25) and preset 4 (was 6) — a single frame makes the slower preset
// effectively free, and it buys real bit efficiency at a fixed CRF. Combined
// with the resolution fix this is roughly 3-7x the bytes of the old output,
// measured across portrait/landscape/ultra-wide sources. If storage ever binds,
// crf 22 keeps most of the win at about 30% less.
//
// pix_fmt stays yuv420p10le. ffmpeg's libsvtav1 wrapper accepts ONLY yuv420p
// and yuv420p10le; asking for yuv444p does not fail, it silently inserts a
// conversion and ships 4:2:0 anyway — the exact bug this function's predecessor
// documented having already made once. Real 4:4:4 needs libaom or avifenc.
//
// avif=1 is still-picture mode, correct for a single frame (which is why
// generateStaticPreview sets it). Never copy it to an animated path — see the
// warning in generateAnimatedAVIF.
func encodeStillAVIF(content []byte, srcExt string) ([]byte, error) {
	return runFFmpeg(content, srcExt, ".avif",
		"-vf", stillScaleCap(stillFullPx),
		"-vframes", "1",
		"-c:v", "libsvtav1",
		"-crf", "20",
		"-preset", "4",
		"-pix_fmt", "yuv420p10le",
		"-color_range", "pc",
		"-svtav1-params", "avif=1",
		"-an",
	)
}

// encodeStillPreviewAVIF is the grid rendition for stills: 1280 long edge, size
// first.
//
// This exists because `original` and `preview` used to be the same object for
// stills, and the frontend loads `original` for image cards — so raising the
// canonical still to 2K without splitting them would have made the masonry grid
// pull multi-megabyte files per tile.
func encodeStillPreviewAVIF(content []byte, srcExt string) ([]byte, error) {
	return runFFmpeg(content, srcExt, ".avif",
		"-vf", stillScaleCap(stillPreviewPx),
		"-vframes", "1",
		"-c:v", "libsvtav1",
		"-crf", "30",
		"-preset", "6",
		"-pix_fmt", "yuv420p10le",
		"-color_range", "pc",
		"-svtav1-params", "avif=1",
		"-an",
	)
}

// avifScaleFilter boxes the AVIF preview and its poster at 720p in either
// orientation: the LONGER edge is capped at 1280, never upscaling, so
// landscape comes out ≤1280×720 and portrait ≤720×1280.
//
// It replaces a fixed 1280×768 box that had the same two defects the WebP
// path (webpScaleFilter) and the stills (stillScaleCap) were already fixed
// for: the asymmetric box was a landscape cap silently applied to portrait
// content (1080×1920 came out 432×768), and a fixed WxH box with
// force_original_aspect_ratio=decrease upscales anything smaller than itself.
// The min(iw,·)/min(ih,·) terms fix the second; the square box fixes the first.
//
// force_divisible_by=2 is required: yuv420p10le needs even dimensions and
// libsvtav1 errors on odd ones, which the old filter left to chance.
//
// This is a much larger portrait budget than the WebP path (720×1280 vs
// 432×768, ~2.8× the pixels). That is deliberate: AVIF decodes through the
// platform's AV1 path, which is hardware-assisted on most devices, whereas
// animated WebP is always software, so the two formats do not share a decode
// budget. AVIF is also not currently the FE's preview format (the bot and FE
// set preview_format=webp), so this path serves non-FE callers and legacy
// records.
const avifScaleFilter = "scale='min(iw,1280)':'min(ih,1280)':force_original_aspect_ratio=decrease:force_divisible_by=2,setsar=1"

// generateStaticPreview extracts the first frame as a static AVIF poster.
// Uses libsvtav1 (CPU) for the same compatibility reason as
// generateAnimatedAVIF — see that function's comment block. Keeping the
// encoder consistent gives one codec path for both previews and canonical
// output.
//
// crf 32 (was 40): a single frame makes bytes cheap, and the poster is what
// a viewer sees while the animation loads, so it is worth being crisp. It
// shares avifScaleFilter with the animated preview so the two agree on size.
func generateStaticPreview(content []byte, srcExt string) ([]byte, error) {
	// avif=1 marks the output as a still-picture AVIF (correct for single
	// frame), and -color_range pc is the AVIF-spec-preferred full range.
	// Both are libavif-team-recommended for canonical AVIF output.
	return runFFmpeg(content, srcExt, ".avif",
		"-vf", avifScaleFilter,
		"-vframes", "1",
		"-c:v", "libsvtav1",
		"-crf", "32",
		"-preset", "6",
		"-pix_fmt", "yuv420p10le",
		"-color_range", "pc",
		"-svtav1-params", "avif=1",
		"-an",
	)
}

// generateAnimatedAVIF creates an animated AVIF preview from video bytes.
//
// Uses libsvtav1 (CPU). The hardware (QSV and VAAPI) paths were both tested
// and both produced AVIFs that play on Apple but fail on Chromium-based
// clients — ffmpeg's avif muxer doesn't correctly handle hardware-encoded
// AV1 streams (libavif issue #2922, "pixi box plane count [0]").
// Software encoding via libsvtav1 produces canonical AVIFs that play
// everywhere.
//
// Settings:
//   - libsvtav1, crf 34, preset 5 (slow preset boosts bit efficiency at fixed CRF)
//     crf was 40. 34 matches the canonical MP4 (transcodeGifToAV1Tuned), so
//     the preview is a re-encode at the same quality target rather than a
//     looser one. Measured on three fancams at the 1280 box: crf has no
//     effect on encode time, only bytes — 40→34 is ~1.45× the file. A 934×1920
//     clip at 622×1280/crf 34 is still ~40% smaller than its WebP preview at
//     374×768/q75.
//   - yuv420p10le (10-bit — AV1/AVIF's design home)
//   - avifScaleFilter (long edge ≤1280, no upscale), fps capped at 30, audio
//     stripped
//   - Full-range color metadata (AVIF-spec preferred)
//
// Typical encode time: ~1.5–4s on an M-series Mac at the 1280 box for 3–9s
// clips (was ~2–3s on i5-10400F at the old 768 box).
//
// If GOYANGI_USE_AVIFENC=1 is set, the call is routed to libavif's reference
// avifenc instead of ffmpeg's avif muxer. See generateAnimatedAVIFViaAvifenc.
func generateAnimatedAVIF(content []byte, srcExt string) ([]byte, error) {
	if os.Getenv("GOYANGI_USE_AVIFENC") == "1" {
		return generateAnimatedAVIFViaAvifenc(content, srcExt)
	}

	const svtav1Params = "tune=0:aq-mode=2:enable-tf=1:film-grain=0:enable-restoration=1:enable-cdef=1:fast-decode=1:tile-columns=1:tile-rows=1:lookahead=60"

	// Note: do NOT add svtav1-params=avif=1 here — that flag puts the
	// encoder in still-picture mode and breaks animated output.
	// -fpsmax 30: Discord's preview renderer caps at ~30fps in feed views,
	// so higher-fps frames are bytes the client never displays. Capping
	// saves ~40-50% file size on 60fps sources with zero visible quality
	// loss in Discord; no-op for sources at ≤30fps.
	return runFFmpeg(content, srcExt, ".avif",
		"-fpsmax", "30",
		"-vf", avifScaleFilter,
		"-c:v", "libsvtav1",
		"-crf", "34",
		"-preset", "5",
		"-pix_fmt", "yuv420p10le",
		"-color_range", "pc",
		"-svtav1-params", svtav1Params,
		"-an",
	)
}
