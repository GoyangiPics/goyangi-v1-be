package hooks

import (
	"fmt"
	"os"
	"path/filepath"
)

// generateAnimatedAVIFViaAvifenc produces an animated AVIF using libavif's
// reference encoder/muxer pipeline instead of ffmpeg's all-in-one path.
//
// Pipeline:
//  1. ffmpeg converts the input MP4 → Y4M (uncompressed frames in a single
//     container file), pre-scaled to the AVIF target box (long edge ≤1280) and
//     converted to yuv420p10le.
//  2. avifenc reads the Y4M and produces a canonical AVIF using libsvtav1
//     under the hood + libavif's reference muxer (the same code Chrome's
//     decoder is built on, so output is guaranteed spec-compliant).
//
// Trade-offs vs the ffmpeg-only path:
//   - Pro: more spec-compliant output, future-proof against decoder strictness
//   - Pro: AVIF-specific tuning knobs (--tune ssim, --speed N)
//   - Con: Y4M intermediate file is large (~50–300 MB depending on duration)
//   - Con: extra binary dependency (libavif-tools)
//   - Con: slightly slower due to disk I/O for the intermediate
//
// Required env: GOYANGI_USE_AVIFENC=1 (toggle on)
// Optional env: GOYANGI_AVIFENC_DIR  (work dir override, defaults to /tmp;
//
//	set to /var/tmp if /tmp is tmpfs)
//
// Required binary: avifenc (Fedora: `sudo dnf install libavif-tools`).
// Verify SVT-AV1 backend support: `avifenc --help | grep -i codec` should
// list `svt`. If only `aom` is available, change `--codec svt` to `--codec aom`
// below — quality is similar, encoding is slower.
func generateAnimatedAVIFViaAvifenc(content []byte, _ string) ([]byte, error) {
	// Work directory — override via GOYANGI_AVIFENC_DIR for production
	// servers where /tmp is tmpfs.
	baseDir := os.Getenv("GOYANGI_AVIFENC_DIR")
	workDir, err := os.MkdirTemp(baseDir, "goyangi-avifenc-*")
	if err != nil {
		return nil, fmt.Errorf("mktemp (base=%q): %w", baseDir, err)
	}
	defer os.RemoveAll(workDir)

	inMP4 := filepath.Join(workDir, "in.mp4")
	if err := os.WriteFile(inMP4, content, 0o644); err != nil {
		return nil, fmt.Errorf("write input mp4: %w", err)
	}

	// Step 1: ffmpeg → Y4M, pre-scaled to AVIF target res + 10-bit YUV.
	// Pre-scaling here keeps the Y4M intermediate small (target res, not
	// source res) and avoids avifenc doing a second scale.
	y4mPath := filepath.Join(workDir, "frames.y4m")
	{
		// Same box as avifScaleFilter (long edge ≤1280, no upscale, even
		// dims) so the two AVIF paths agree on output size. UNTESTED here:
		// this path is off in production and no avifenc binary was available
		// when the box was changed.
		const scaleAndFmt = "scale='min(iw,1280)':'min(ih,1280)':force_original_aspect_ratio=decrease:force_divisible_by=2:flags=lanczos,setsar=1,format=yuv420p10le"
		// fpsmax 30 — match the ffmpeg path; Discord caps previews around
		// 30fps so anything above is wasted bytes.
		if err := runEncoder("ffmpeg",
			"-i", inMP4,
			"-fpsmax", "30",
			"-vf", scaleAndFmt,
			"-an",
			"-strict", "experimental", // for 10-bit Y4M
			"-y", y4mPath,
		); err != nil {
			return nil, fmt.Errorf("y4m extract: %w", err)
		}
	}

	// Step 2: avifenc → AVIF using SVT-AV1 backend.
	// --qcolor 40 approximates CRF 40 on the libsvtav1 backend (the same
	// quality target as the ffmpeg path uses).
	//
	// Note: --tune is intentionally omitted. Per libavif docs, --tune is
	// "AOM-SPECIFIC" — it's silently ignored on --codec svt. To actually
	// bias SVT-AV1 toward SSIM, use `--advanced tune=2` (SVT's own tune
	// API, where 0=VQ, 1=PSNR, 2=SSIM). For preview AVIFs at high CRF
	// the perceptual difference is marginal; default tuning is fine.
	outPath := filepath.Join(workDir, "out.avif")
	{
		if err := runEncoder("avifenc",
			"--codec", "svt",
			"--depth", "10",
			"--yuv", "420",
			"--range", "full",
			"--qcolor", "40",
			"--speed", "5",
			y4mPath, outPath,
		); err != nil {
			return nil, fmt.Errorf("avifenc: %w", err)
		}
	}

	return os.ReadFile(outPath)
}
