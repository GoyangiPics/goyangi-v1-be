package hooks

import (
	"context"
	"log"
	"os/exec"
	"slices"
	"strings"
	"time"

	"goyangi-v1-be/remoteenc"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

// requiredEncoders are the ffmpeg encoders the upload pipeline cannot work
// without — see remoteenc.RequiredEncoders, which the encode worker checks too.
var requiredEncoders = remoteenc.RequiredEncoders

// RegisterPreflight checks the external tools the encode pipeline shells out
// to, once at boot, and says plainly in the log what is missing.
//
// The server may run on a machine its maintainer can't reach, so a broken
// ffmpeg install has to be obvious from the first lines of the log rather than
// surfacing as a failed upload later. It warns and never stops the server:
// browsing and the API work without ffmpeg, only encoding doesn't.
//
// It also resolves the H.264 encoder (h264Encoder), so the NVENC test encode
// runs now and its result is logged next to the rest.
func RegisterPreflight(app *pocketbase.PocketBase) {
	app.OnServe().BindFunc(func(e *core.ServeEvent) error {
		// Off the serve path: the checks spawn ffmpeg a couple of times.
		go runPreflight()
		return e.Next()
	})
}

func runPreflight() {
	var missing []string

	// Also wakes a sleeping worker, so the first upload doesn't pay the boot.
	if c := remoteEncoder(); c != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		start := time.Now()
		health, err := c.Health(ctx)
		cancel()
		if err != nil {
			log.Printf("❌ preflight: encode worker unreachable: %v", err)
		} else {
			log.Printf("🛰️  preflight: encode worker up in %s: %s", time.Since(start).Round(time.Millisecond), health)
		}
	}

	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			log.Printf("❌ preflight: %s not found on PATH", bin)
			missing = append(missing, bin)
			continue
		}
		log.Printf("🔎 preflight: %s → %s", bin, path)
	}

	if !slices.Contains(missing, "ffmpeg") {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		out, err := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-encoders").Output()
		cancel()
		if err != nil {
			log.Printf("❌ preflight: ffmpeg -encoders failed: %v", err)
			missing = append(missing, "ffmpeg encoders")
		} else {
			for _, enc := range requiredEncoders {
				if !strings.Contains(string(out), " "+enc+" ") {
					log.Printf("❌ preflight: ffmpeg lacks the %s encoder", enc)
					missing = append(missing, enc)
				}
			}
		}
		h264Encoder()
	}

	if len(missing) > 0 {
		log.Printf("❌ preflight: missing %s — uploads that need it will fail until this is fixed",
			strings.Join(missing, ", "))
		return
	}
	log.Printf("✅ preflight ok: ffmpeg/ffprobe found with %s; H.264 SD on %s",
		strings.Join(requiredEncoders, ", "), h264Encoder().kind)
}
