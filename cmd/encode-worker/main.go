// Command encode-worker runs the backend's ffmpeg encodes on request — see
// package remoteenc. EXPERIMENT (exp branch): deployed to Railway to measure
// whether a pay-per-use worker is a viable home for the encode load.
//
// Environment:
//
//	PORT                  listen port (Railway sets it; 8080 otherwise)
//	WORKER_TOKEN          required; the backend's ENCODE_WORKER_TOKEN
//	WORKER_MAX_JOBS       concurrent ffmpeg runs (default 4)
//	R2_ENDPOINT           https://<account>.r2.cloudflarestorage.com
//	R2_BUCKET             the bucket PocketBase stores uploads in
//	R2_ACCESS_KEY_ID      a read-only R2 token is enough
//	R2_SECRET_ACCESS_KEY
//	R2_REGION             default "auto"
//
// Without the R2_* variables the worker still runs, but the backend has to send
// every source in the request body.
package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"goyangi-v1-be/remoteenc"

	"github.com/pocketbase/pocketbase/tools/filesystem"
)

func main() {
	token := os.Getenv("WORKER_TOKEN")
	if token == "" {
		log.Fatal("❌ WORKER_TOKEN is required")
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	ffmpegVersion := checkFFmpeg()
	cpuQuota := readCPUQuota()
	log.Printf("🖥️  host CPUs %d, cgroup CPU quota %s", runtime.NumCPU(), cpuQuota)

	var sources remoteenc.SourceFetcher
	if bucket := os.Getenv("R2_BUCKET"); bucket != "" {
		region := os.Getenv("R2_REGION")
		if region == "" {
			region = "auto"
		}
		fs, err := filesystem.NewS3(bucket, region, os.Getenv("R2_ENDPOINT"),
			os.Getenv("R2_ACCESS_KEY_ID"), os.Getenv("R2_SECRET_ACCESS_KEY"), true)
		if err != nil {
			log.Fatalf("❌ R2: %v", err)
		}
		defer fs.Close()
		sources = r2Sources{fs}
		log.Printf("🪣 R2: sources fetched from bucket %s", bucket)
	} else {
		log.Printf("⚠️  R2 not configured — sources must arrive in the request body")
	}

	maxJobs, _ := strconv.Atoi(os.Getenv("WORKER_MAX_JOBS"))
	srv := &remoteenc.Server{
		Token:   token,
		Sources: sources,
		MaxJobs: maxJobs,
		Health: func() any {
			return map[string]any{
				"ffmpeg":   ffmpegVersion,
				"hostCPUs": runtime.NumCPU(),
				"cpuQuota": cpuQuota,
			}
		},
	}

	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}

	// On SIGTERM (redeploy, sleep) let in-flight encodes finish first. main
	// waits for that: ListenAndServe returns as soon as Shutdown starts.
	// Railway kills the process after RAILWAY_DEPLOYMENT_DRAINING_SECONDS.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		log.Printf("🛑 encode worker: shutting down once in-flight encodes finish")
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := httpSrv.Shutdown(shutdown); err != nil {
			log.Printf("⚠️  encode worker: shutdown: %v", err)
		}
	}()

	log.Printf("🛰️  encode worker listening on :%s", port)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-drained
}

// checkFFmpeg fails the boot when ffmpeg or a required encoder is missing — a
// worker that can't encode should never pass the platform's health check.
func checkFFmpeg() string {
	ver, err := exec.Command("ffmpeg", "-hide_banner", "-version").Output()
	if err != nil {
		log.Fatalf("❌ ffmpeg: %v", err)
	}
	encoders, err := exec.Command("ffmpeg", "-hide_banner", "-encoders").Output()
	if err != nil {
		log.Fatalf("❌ ffmpeg -encoders: %v", err)
	}
	for _, enc := range remoteenc.RequiredEncoders {
		if !strings.Contains(string(encoders), " "+enc+" ") {
			log.Fatalf("❌ ffmpeg lacks the %s encoder", enc)
		}
	}
	first, _, _ := strings.Cut(string(ver), "\n")
	log.Printf("🎬 %s (%s ok)", first, strings.Join(remoteenc.RequiredEncoders, ", "))
	return first
}

// readCPUQuota reports the cgroup v2 CPU limit — what the container may
// actually use, which runtime.NumCPU (the host's count) doesn't show.
func readCPUQuota() string {
	b, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	if err != nil {
		return "unknown"
	}
	fields := strings.Fields(string(b))
	if len(fields) != 2 || fields[0] == "max" {
		return "unlimited"
	}
	quota, err1 := strconv.ParseFloat(fields[0], 64)
	period, err2 := strconv.ParseFloat(fields[1], 64)
	if err1 != nil || err2 != nil || period == 0 {
		return strings.TrimSpace(string(b))
	}
	return strconv.FormatFloat(quota/period, 'f', 1, 64) + " vCPU"
}

type r2Sources struct{ fs *filesystem.System }

func (s r2Sources) Fetch(_ context.Context, key string, w io.Writer) error {
	r, err := s.fs.GetReader(key)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(w, r)
	return err
}
