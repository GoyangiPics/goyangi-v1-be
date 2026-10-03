package hooks

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"
)

// encodeTimeout bounds any single media-encoding subprocess. Without it a
// malformed or pathological input can hang ffmpeg/avifenc forever; combined
// with the (default size 1) process semaphore that would permanently wedge
// the whole upload pipeline.
const encodeTimeout = 5 * time.Minute

// runEncoder runs an encoder subprocess with a hard timeout, capturing stderr
// for error reporting. Returns a clear error on timeout.
func runEncoder(name string, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), encodeTimeout)
	defer cancel()

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s timed out after %s", name, encodeTimeout)
		}
		return fmt.Errorf("%w\n%s", err, stderr.String())
	}
	return nil
}

// probeTimeout bounds metadata probes (ffprobe), which should be near-instant.
const probeTimeout = 30 * time.Second

// runProbe runs a probe subprocess with a short timeout and returns its stdout.
func runProbe(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).Output()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%s timed out after %s", name, probeTimeout)
	}
	return out, err
}
