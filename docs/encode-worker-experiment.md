# Encode worker experiment (exp branch)

Question: can ffmpeg move off the server onto a pay-per-use worker — and how
fast, how cheap? The backend and frontend run locally; every ffmpeg encode goes
to `cmd/encode-worker` on Railway instead.

## How it works

- `runFFmpegWith` (hooks/ffmpeg.go) is the single place every encode passes
  through. With `ENCODE_WORKER_URL` set it sends the same ffmpeg arguments to
  the worker (hooks/remote_encode.go) instead of running them locally.
- The upload source is already in R2, so the worker fetches it by key and keeps
  it for 5 minutes. The AV1 MP4 and the H.264 SD rendition both read it, and
  neither pushes ~100 MB through the backend's connection. Smaller inputs
  (the intermediate MP4 the previews are cut from, `/api/convert` uploads) go in
  the request body.
- H.264 SD uses libx264 when the worker is on; the worker has no GPU.
- Still local: ffprobe, the RIFE interpolation path, and the avifenc path
  (`GOYANGI_USE_AVIFENC`, off in production).
- The worker endpoint runs any ffmpeg arguments it is given, guarded only by the
  bearer token. That's fine for the experiment, but production should expose
  named operations.

## Railway setup

1. New project → empty service, region **EU West (Amsterdam)**.
2. Service variables:

   | variable | value |
   |---|---|
   | `RAILWAY_DOCKERFILE_PATH` | `Dockerfile.worker` |
   | `WORKER_TOKEN` | `openssl rand -hex 32` |
   | `R2_ENDPOINT` | `https://<account-id>.r2.cloudflarestorage.com` |
   | `R2_BUCKET` | the bucket PocketBase stores uploads in |
   | `R2_ACCESS_KEY_ID` / `R2_SECRET_ACCESS_KEY` | an R2 API token, **Object Read** on that bucket only |
   | `WORKER_MAX_JOBS` | optional, default 4 |

3. Settings → Networking → **Generate Domain**. The worker listens on `$PORT`.
4. Settings → **Serverless** on: the service sleeps after 10 minutes without
   traffic and wakes on the next request.
5. Settings → resource limits: give it the most vCPU your plan allows. The
   boot log shows what it actually got (`cgroup CPU quota`).
6. Deploy from the repo root on `exp`: `railway link`, then `railway up`. This
   uploads the working tree, so nothing has to be pushed.

A healthy boot logs the ffmpeg version, `libsvtav1, libx264, libwebp, aac ok`,
the CPU quota, and `encode worker listening`. If an encoder is missing, the
worker exits instead.

## Local backend

```
ENCODE_WORKER_URL=https://<service>.up.railway.app
ENCODE_WORKER_TOKEN=<same as WORKER_TOKEN>
```

At boot, preflight calls the worker's `/healthz`. This also wakes it, and it
logs `encode worker up in …` with the worker's ffmpeg version and CPUs.
Fetching sources by key needs PocketBase's storage to be R2 (Settings → Files
storage → S3). Otherwise every source goes in the request body.

## Reading the numbers

Every encode logs a `📊` line, so `grep 📊` on the backend log is the result set.

```
📊 encode[remote] transcodeGifToAV1Tuned src=r2 out=845KB total=6.2s worker=5.1s fetch=0.4s overhead=0.7s cpu=38.2s (7.5 cores) rss=1.20GB ≈$0.00031 · session: 14 remote, cpu 512.0s, ≈$0.0043
📊 encode[local]  transcodeGifToAV1Tuned wall=4.8s cpu=52.1s (10.9 cores) rss=1.10GB · session: 14 local, cpu 610.0s, wall 71.0s
```

- **worker vs wall:** encode speed there versus here, op by op.
- **cores:** how much CPU the worker really got. On shared CPU this shows
  contention.
- **overhead:** moving inputs and outputs, waiting for a job slot, and boot time
  when the line says `COLD`.
- **≈$:** Railway's per-minute rates. CPU is exact. Memory is an upper bound
  (peak memory charged for the whole run). Rates can be overridden with
  `RAILWAY_VCPU_MIN_USD` and `RAILWAY_GB_MIN_USD`.

## Test plan

1. **Baseline.** Leave `ENCODE_WORKER_URL` unset and upload a burst of ~15 real
   clips. Note the session totals.
2. **Remote.** Set the variables, restart, and upload the same burst.
3. **Cold start.** Leave the worker alone for more than 10 minutes, then upload
   one clip. The first line says `COLD`, and its overhead is the wake-up cost.
4. **Variance.** Repeat step 2 at a few different times of day. Shared CPU is
   the main risk.
5. **Bill check.** A day later, compare the session ≈$ against Railway's usage
   page, which also includes the worker's idle memory while awake.

Compare the ffmpeg versions too. The worker image ships Alpine's ffmpeg (6.1,
SVT-AV1 2.x), and a newer SVT-AV1 locally can be faster per frame on its own.
