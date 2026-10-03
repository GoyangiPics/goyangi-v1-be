package hooks

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

// H.264 "SD" rendition — the compatibility fallback stored alongside the
// canonical AV1 MP4 (the `sd` field / `-sd.mp4` object).
//
// Why it exists: Safari never software-decodes AV1. Apple devices below
// A17 Pro / M3 don't degrade on an AV1 MP4, they render nothing — which is
// the blank-video report. H.264 High profile, 8-bit 4:2:0, in MP4 is the only
// combination with a hardware decoder on effectively every phone, desktop and
// TV still in use, so it is what the fallback has to be.
//
// Why not VP9/WebM, the other obvious candidate: the A310 decodes VP9 but
// cannot encode it, and Apple has no VP9 hardware decoder at all — a VP9
// fallback would land back in software decode, which is the same battery and
// stutter problem this rendition exists to avoid.
//
// Why the GPU is safe here when the AV1/AVIF work is pinned to CPU: the bug
// that keeps those on CPU (libavif #2922, see generateAnimatedAVIF) is in
// ffmpeg's *avif muxer* mishandling hardware-encoded AV1 streams. Plain
// H.264-into-MP4 never touches that code path.

// sdVAAPIQuality is the constant-quantizer target for the hardware encoder
// (h264_vaapi -rc_mode CQP). Lower is better quality and bigger files; 24 at
// 720p is a deliberately safe point — this rendition is watched by the people
// whose devices can't play anything better, so it is not the place to chase
// bytes the way the AV1 and preview encodes do.
const sdVAAPIQuality = "24"

// sdCPUQuality is the libx264 fallback's CRF. Not directly comparable to the
// VAAPI QP above — hardware rate control is less efficient at a given nominal
// quantizer, so the software path is set slightly tighter to land in roughly
// the same visual place.
const sdCPUQuality = "23"

// sdNVENCQuality is h264_nvenc's constant-quality target (-rc vbr -cq N -b:v 0).
const sdNVENCQuality = "23"

// scaleDown720p boxes the SD rendition at 1280×720. Sources already at or
// below the cap are left alone rather than upscaled.
//
// The box stays asymmetric ON PURPOSE, unlike scaleDown1080p's, which went
// square so portrait content gets its full height. SD is the budget rendition —
// old devices and constrained bandwidth — so portrait binding on the 720 height
// cap (1080×1920 → 405×720) is its size budget at work, not the landscape-cap
// bug the AV1 box had. Anyone wanting portrait at full height has the AV1
// original; this is the rendition for when that one doesn't play.
//
// force_divisible_by=2 is required because yuv420p/nv12 need even dimensions,
// and portrait sources (most fancams — they bind on the height cap as above)
// otherwise land on an odd width and the encoder errors.
const scaleDown720p = "scale='min(iw,1280)':'min(ih,720)':force_original_aspect_ratio=decrease:force_divisible_by=2,setsar=1"

// H.264 encoder kinds, as resolved by h264Encoder.
const (
	h264VAAPI = "vaapi" // Intel/AMD media engine via a DRM render node (Linux)
	h264NVENC = "nvenc" // NVIDIA media engine (Windows or Linux)
	h264CPU   = "cpu"   // libx264
)

type h264EncoderChoice struct {
	kind   string
	device string // VAAPI render node; empty for the other kinds
}

// h264Encoder picks the SD rendition's encoder, once, on first use (the boot
// preflight calls it so the choice and its probe happen at startup, not on the
// first upload).
//
// GOYANGI_H264_ENCODER selects it: "auto" (default), "vaapi", "nvenc" or "cpu".
// auto prefers VAAPI when the render node exists (the Fedora/Arc setup), then
// NVENC when a test encode succeeds (Windows/NVIDIA), then libx264.
// GOYANGI_VAAPI_DEVICE overrides the render node; "off" still forces the CPU
// path, as it did before NVENC existed.
//
// The NVENC check is a real encode rather than a look at `ffmpeg -encoders`:
// common Windows builds list h264_nvenc whether or not an NVIDIA GPU and
// driver are present.
var h264Encoder = sync.OnceValue(func() h264EncoderChoice {
	want := strings.ToLower(strings.TrimSpace(os.Getenv("GOYANGI_H264_ENCODER")))
	dev := os.Getenv("GOYANGI_VAAPI_DEVICE")
	if dev == "off" {
		want = h264CPU
	}
	if dev == "" || dev == "off" {
		dev = "/dev/dri/renderD128"
	}

	cpu := func(why string) h264EncoderChoice {
		log.Printf("⚠️  H.264: %s — SD rendition encodes on CPU (libx264), "+
			"which will contend with the AV1 encode", why)
		return h264EncoderChoice{kind: h264CPU}
	}
	vaapi := func() (h264EncoderChoice, error) {
		if _, err := os.Stat(dev); err != nil {
			return h264EncoderChoice{}, err
		}
		log.Printf("🎬 H.264: VAAPI device %s — SD rendition encodes on GPU", dev)
		return h264EncoderChoice{kind: h264VAAPI, device: dev}, nil
	}
	nvenc := func() (h264EncoderChoice, error) {
		if err := probeNVENC(); err != nil {
			return h264EncoderChoice{}, err
		}
		log.Printf("🎬 H.264: NVENC — SD rendition encodes on GPU")
		return h264EncoderChoice{kind: h264NVENC}, nil
	}

	switch want {
	case h264CPU:
		log.Printf("🔧 H.264: CPU encoder forced by config — SD rendition encodes with libx264")
		return h264EncoderChoice{kind: h264CPU}
	case h264VAAPI:
		c, err := vaapi()
		if err != nil {
			return cpu(fmt.Sprintf("VAAPI requested but %s unavailable (%v)", dev, err))
		}
		return c
	case h264NVENC:
		c, err := nvenc()
		if err != nil {
			return cpu(fmt.Sprintf("NVENC requested but its test encode failed (%v)", firstLine(err)))
		}
		return c
	case "", "auto":
		if c, err := vaapi(); err == nil {
			return c
		}
		if c, err := nvenc(); err == nil {
			return c
		}
		return cpu("no GPU encoder found (no VAAPI device, NVENC test encode failed)")
	default:
		return cpu(fmt.Sprintf("unknown GOYANGI_H264_ENCODER %q", want))
	}
})

// probeNVENC runs a fraction of a second of synthetic video through h264_nvenc.
// It fails on a build without the encoder and on a machine without a working
// NVIDIA GPU/driver alike, which is exactly the question being asked.
func probeNVENC() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=256x256:d=0.2",
		"-c:v", "h264_nvenc", "-f", "null", "-")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// firstLine trims an ffmpeg error to its first line for a one-line boot log.
func firstLine(err error) string {
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// transcodeToH264SD produces the ≤720p H.264 MP4 fallback, preferring the
// GPU's media engine (Arc via VAAPI, NVIDIA via NVENC) and dropping to libx264
// if the hardware path is absent or fails.
//
// The fallback is deliberately not latched: a single failure is far more
// likely to be one pathological input than a broken driver, and permanently
// disabling the GPU on the strength of one bad file would quietly move every
// later upload onto the CPU — the exact contention this rendition was put on
// the GPU to avoid. A genuinely broken driver shows up as a repeated warning
// in the log instead.
func transcodeToH264SD(content []byte, srcExt string, keepAudio bool, meta []string) ([]byte, error) {
	enc := h264Encoder()
	var (
		out []byte
		err error
	)
	switch enc.kind {
	case h264VAAPI:
		out, err = encodeH264VAAPI(content, srcExt, keepAudio, enc.device, meta)
	case h264NVENC:
		out, err = encodeH264NVENC(content, srcExt, keepAudio, meta)
	default:
		return encodeH264CPU(content, srcExt, keepAudio, meta)
	}
	if err == nil {
		return out, nil
	}
	log.Printf("⚠️  H.264: %s encode failed, retrying this file on CPU: %v", strings.ToUpper(enc.kind), err)
	return encodeH264CPU(content, srcExt, keepAudio, meta)
}

// encodeH264VAAPI encodes on the Arc's media engine (any VAAPI device).
//
// Decode and scale stay in software (note the plain scale filter followed by
// hwupload, rather than -hwaccel vaapi with a full hardware pipeline). That
// is on purpose: uploads arrive in whatever the source happened to be, and a
// full hardware pipeline fails outright on anything the Arc's decoder doesn't
// handle, leaving the record with no fallback at all. Software decode works
// for every input, and it is the *encode* — the expensive half — that this
// path exists to move off the CPU.
//
// -profile:v high with nv12 pins the output to 8-bit 4:2:0, keeping it clear
// of High 10 and 4:2:2, which are the two ways an H.264 file loses the
// universal hardware decode that is the whole point of this rendition.
//
// No -level is set: 720p sits comfortably inside Level 4.0 whatever the
// driver picks, and a rejected -level value would send the file to the CPU
// path for nothing.
func encodeH264VAAPI(content []byte, srcExt string, keepAudio bool, device string, meta []string) ([]byte, error) {
	args := []string{
		"-vf", scaleDown720p + ",format=nv12,hwupload",
		"-c:v", "h264_vaapi",
		"-rc_mode", "CQP",
		"-qp", sdVAAPIQuality,
		"-profile:v", "high",
		"-g", "60",
	}
	args = append(args, h264AudioArgs(keepAudio)...)
	args = append(args, meta...)
	// use_metadata_tags keeps the goyangi_* keys — see filemeta.go.
	args = append(args, "-movflags", "+faststart+use_metadata_tags")

	return runFFmpegWith(content, srcExt, ".mp4", []string{"-vaapi_device", device}, args)
}

// encodeH264NVENC encodes on an NVIDIA GPU's media engine.
//
// Same shape as the VAAPI path: decode and scale stay in software and only the
// encode moves to the GPU (h264_nvenc accepts system-memory frames, so no
// hwupload is needed). -rc vbr with -cq and -b:v 0 is NVENC's constant-quality
// mode, the counterpart of libx264's CRF; 23 lands near the other two paths.
// The profile and yuv420p pin it to 8-bit 4:2:0 High for the same universal-
// decode reason given on encodeH264VAAPI.
func encodeH264NVENC(content []byte, srcExt string, keepAudio bool, meta []string) ([]byte, error) {
	args := []string{
		"-vf", scaleDown720p + ",format=yuv420p",
		"-c:v", "h264_nvenc",
		"-preset", "p5",
		"-tune", "hq",
		"-rc", "vbr",
		"-cq", sdNVENCQuality,
		"-b:v", "0",
		"-profile:v", "high",
		"-g", "60",
	}
	args = append(args, h264AudioArgs(keepAudio)...)
	args = append(args, meta...)
	args = append(args, "-movflags", "+faststart+use_metadata_tags")

	return runFFmpeg(content, srcExt, ".mp4", args...)
}

// encodeH264CPU is the software fallback.
//
// preset veryfast, not something slower: this path only runs when the GPU is
// unavailable, which means it is competing with libsvtav1 for the same cores.
// A slower preset would buy a few percent of file size while measurably
// delaying the canonical AV1 encode, which is the wrong trade for a fallback
// rendition.
func encodeH264CPU(content []byte, srcExt string, keepAudio bool, meta []string) ([]byte, error) {
	args := []string{
		"-vf", scaleDown720p,
		"-c:v", "libx264",
		"-preset", "veryfast",
		"-crf", sdCPUQuality,
		"-profile:v", "high",
		"-pix_fmt", "yuv420p",
		"-g", "60",
	}
	args = append(args, h264AudioArgs(keepAudio)...)
	args = append(args, meta...)
	args = append(args, "-movflags", "+faststart+use_metadata_tags")

	return runFFmpeg(content, srcExt, ".mp4", args...)
}

// h264AudioArgs keeps audio as AAC-LC stereo (the only audio codec with the
// same universal decode support as the video track) or strips it entirely.
func h264AudioArgs(keepAudio bool) []string {
	if keepAudio {
		return []string{"-c:a", "aac", "-b:a", "128k", "-ac", "2"}
	}
	return []string{"-an"}
}

// h264Job is an SD encode running in the background, so the upload pipeline
// can put it on the GPU and get on with the CPU work in the meantime.
type h264Job struct {
	wg    sync.WaitGroup
	bytes []byte
	err   error
}

// startH264SD begins an SD encode and returns immediately.
//
// This does not take a slot from processSem. The semaphore admits one *record*
// at a time, and this job belongs to the record that already holds the slot —
// gating it separately would deadlock at the default size of 1.
func startH264SD(content []byte, srcExt string, keepAudio bool, meta []string) *h264Job {
	j := &h264Job{}
	j.wg.Add(1)
	go func() {
		defer j.wg.Done()
		j.bytes, j.err = transcodeToH264SD(content, srcExt, keepAudio, meta)
	}()
	return j
}

// wait blocks until the encode finishes. Nil-safe: a job that was never
// started reports no bytes and no error, which uploadSD treats as "skip".
func (j *h264Job) wait() ([]byte, error) {
	if j == nil {
		return nil, nil
	}
	j.wg.Wait()
	return j.bytes, j.err
}

// uploadSD stores the H.264 fallback under <keyBase>-sd.mp4 and points the
// record's `sd` field at it.
//
// Best-effort throughout, like `static`: a record with a working AV1 MP4 and
// no fallback is degraded but usable, whereas failing the record outright
// would lose content over a rendition that most viewers never request.
//
// `put` rather than a *filesystem.System so the object joins the caller's
// rollback set — an SD rendition uploaded before a later failure would otherwise
// be orphaned with nothing pointing at it.
func uploadSD(put func(content []byte, key string) error, record *core.Record, sdBytes []byte, sdErr error, r2BaseURL, keyBase string) {
	if sdErr != nil {
		log.Printf("⚠️  R2: SD (H.264) encode failed: %v", sdErr)
		return
	}
	if len(sdBytes) == 0 {
		return // job never started
	}
	sdKey := keyBase + "-sd.mp4"
	if err := put(sdBytes, sdKey); err != nil {
		log.Printf("⚠️  R2: SD upload %s failed: %v", sdKey, err)
		return
	}
	record.Set("sd", r2BaseURL+"/"+sdKey)
}
