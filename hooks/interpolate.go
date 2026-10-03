package hooks

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// InterpolateMP4 runs RIFE frame interpolation on an already-encoded MP4
// (output of transcodeGifToAV1Tuned) and returns a new MP4 with extra frames
// inserted.
//
// mode determines what the extra frames are used for:
//   - "slow"   (default): encode at source fps → longer duration, same playback speed
//     e.g. factor=2 turns a 5s clip into 10s slow-motion
//   - "smooth"          : encode at source fps × factor → same duration, higher fps
//     e.g. factor=2 on a 60fps source produces 120fps output
//     (useful for already-slowed-down source content)
//
// Requires `rife-ncnn-vulkan` binary in PATH. Uses the Intel Arc A310 via
// Vulkan — completely separate code path from QSV/VAAPI, so it's not
// affected by the AVIF muxer bugs we hit with hardware AV1 encode.
//
// Pipeline:
//  1. ffmpeg extracts source frames to a temp directory as PNG (lossless,
//     preserves quality through the interpolation step)
//  2. rife-ncnn-vulkan interpolates to a target frame count. Source frames
//     are deleted right after RIFE finishes to cap peak disk usage.
//  3. ffmpeg re-encodes interpolated frames back to MP4+AV1 at source fps
//     (so playback at source fps = slowdown effect)
//
// Disk usage: peak ~1.5-3 GB for a 5s 1080p clip at 2x (PNG frames are big).
// IMPORTANT: set GOYANGI_RIFE_DIR=/var/tmp on production servers if /tmp is
// tmpfs / has limited space — /var/tmp is on real disk and won't eat RAM.
//
// Performance on Arc A310 for a 5s 1080p clip:
//   - factor 1.25 → ~6–10s
//   - factor 1.5  → ~10–15s
//   - factor 2.0  → ~13–20s
//
// maxInterpolateFactor caps the interpolation multiplier. The UI only offers
// 1.25/1.5/2.0; this is a defensive bound so a hand-crafted upload can't drive
// RIFE into writing an unbounded number of PNG frames (disk/OOM DoS).
const maxInterpolateFactor = 4.0

// maxInterpolatedFrames hard-caps the absolute output frame count regardless
// of factor, bounding peak disk use for very long sources.
const maxInterpolatedFrames = 4000

func InterpolateMP4(mp4Bytes []byte, factor float64, mode string) ([]byte, error) {
	if factor <= 1.0 {
		return mp4Bytes, nil // no-op
	}
	if factor > maxInterpolateFactor {
		factor = maxInterpolateFactor
	}
	if mode == "" {
		mode = "slow" // default for backward compat
	}

	// Allow override of the work directory for environments where /tmp has
	// quota limits. Production servers should point this at a path with
	// plenty of free space (e.g., /var/tmp or a dedicated cache mount).
	baseDir := os.Getenv("GOYANGI_RIFE_DIR")
	workDir, err := os.MkdirTemp(baseDir, "goyangi-rife-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp (base=%q): %w", baseDir, err)
	}
	defer os.RemoveAll(workDir)

	inDir := filepath.Join(workDir, "in")
	outDir := filepath.Join(workDir, "out")
	if err := os.MkdirAll(inDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}

	// Write the source MP4 to disk so ffmpeg + ffprobe can read it.
	inMP4 := filepath.Join(workDir, "in.mp4")
	if err := os.WriteFile(inMP4, mp4Bytes, 0o644); err != nil {
		return nil, fmt.Errorf("write input mp4: %w", err)
	}

	// Probe source for framerate + frame count.
	fps, frameCount, err := probeFpsAndFrameCount(inMP4)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	if frameCount < 2 {
		return mp4Bytes, nil // nothing to interpolate
	}

	targetFrames := int(float64(frameCount) * factor)
	if targetFrames <= frameCount {
		return mp4Bytes, nil // factor effectively 1
	}
	if targetFrames > maxInterpolatedFrames {
		return nil, fmt.Errorf("interpolation target %d exceeds cap %d", targetFrames, maxInterpolatedFrames)
	}

	// Step 1: extract frames as PNG (lossless).
	if err := runEncoder("ffmpeg",
		"-i", inMP4,
		"-f", "image2",
		filepath.Join(inDir, "%08d.png"),
	); err != nil {
		return nil, fmt.Errorf("extract frames: %w", err)
	}

	// Step 2: RIFE interpolates to target frame count on the GPU (Vulkan).
	if err := runEncoder("rife-ncnn-vulkan",
		"-i", inDir,
		"-o", outDir,
		"-n", strconv.Itoa(targetFrames),
	); err != nil {
		return nil, fmt.Errorf("rife: %w", err)
	}

	// Free disk: source frames aren't needed past this point.
	_ = os.RemoveAll(inDir)

	// Step 3: re-encode interpolated frames back to MP4.
	// - "slow" mode: encode at source fps → more frames at same fps = longer duration
	// - "smooth" mode: encode at fps×factor → same duration, higher framerate
	outFps := fps
	if mode == "smooth" {
		outFps = fps * factor
	}
	outMP4 := filepath.Join(workDir, "out.mp4")
	if err := runEncoder("ffmpeg",
		"-framerate", fmt.Sprintf("%.6f", outFps),
		"-i", filepath.Join(outDir, "%08d.png"),
		"-vf", scaleDown1080p,
		"-c:v", "libsvtav1",
		"-crf", "34",
		"-preset", "6",
		"-pix_fmt", "yuv420p10le",
		"-svtav1-params", "tune=0:aq-mode=2:enable-tf=1:film-grain=0:lookahead=60",
		"-an",
		"-movflags", "+faststart",
		"-y", outMP4,
	); err != nil {
		return nil, fmt.Errorf("re-encode: %w", err)
	}

	return os.ReadFile(outMP4)
}

// probeFpsAndFrameCount returns the source framerate and total frame count.
// Falls back to a packet count if nb_frames is unavailable (common for
// formats that don't store it in the container).
func probeFpsAndFrameCount(path string) (float64, int, error) {
	out, err := runProbe("ffprobe",
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=r_frame_rate,nb_frames",
		"-of", "csv=p=0",
		path,
	)
	if err != nil {
		return 0, 0, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) < 1 {
		return 0, 0, fmt.Errorf("unexpected probe output: %q", out)
	}

	fps, err := parseRationalFps(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse fps %q: %w", parts[0], err)
	}

	var count int
	if len(parts) > 1 {
		count, _ = strconv.Atoi(parts[1])
	}
	if count == 0 {
		// Fallback: count packets (slower but reliable).
		out2, err := runProbe("ffprobe",
			"-v", "error",
			"-select_streams", "v:0",
			"-count_packets",
			"-show_entries", "stream=nb_read_packets",
			"-of", "csv=p=0",
			path,
		)
		if err != nil {
			return fps, 0, err
		}
		count, _ = strconv.Atoi(strings.TrimSpace(string(out2)))
	}

	return fps, count, nil
}

// parseRationalFps parses "24000/1001" or "30/1" or "24" into a float.
func parseRationalFps(s string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("empty fps")
	}
	if !strings.Contains(s, "/") {
		return strconv.ParseFloat(s, 64)
	}
	parts := strings.SplitN(s, "/", 2)
	num, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0, err
	}
	den, err := strconv.ParseFloat(parts[1], 64)
	if err != nil {
		return 0, err
	}
	if den == 0 {
		return 0, fmt.Errorf("zero denominator")
	}
	return num / den, nil
}
