# Hosting the backend on Railway

This moves the backend off the Windows host (docs/self-host-windows.md) and onto
Railway. Media stays in R2 and the frontend stays on Cloudflare. Only
`api.goyangi.pics` changes where it points.

One Railway project holds two services, both built from this repo:

| | **backend** | **worker** |
|---|---|---|
| What | PocketBase + the Discord bot | ffmpeg encodes (`cmd/encode-worker`) |
| Dockerfile | `Dockerfile` | `Dockerfile.worker` |
| Runs | Always on. The bot holds a Discord connection. | Serverless: sleeps when idle, wakes on the next encode |
| Storage | Volume at `/app/pb_data` | None |
| Reachable | Public at `api.goyangi.pics` | Private network only, no public domain |

The backend calls the worker at `worker.railway.internal`. Nothing outside the
project can reach the worker. That is also why its "run these ffmpeg
arguments" endpoint is acceptable without named operations.

Both run FFmpeg 9.0 from the same BtbN build. Neither has a GPU, so the H.264
SD rendition encodes with libx264 on the worker. RIFE isn't available.

---

## 1. Project

1. Create a new empty project.
2. Use the region **EU West (Amsterdam)** for both services. The worker is
   already there, and R2 is in the EU.
3. Check the plan. A volume's default size is 5 GB on Hobby and 50 GB on Pro.
   `pb_data` holds only the databases, since media is in R2. Check its size on
   the Windows host first (`data.db` + `auxiliary.db`).

## 2. Worker service

Settings:

- **Source:** this repo (see §4).
- **Serverless:** on.
- **Networking:** don't generate a public domain.
- **Healthcheck path:** `/healthz`. It answers `ok` without the token.
- **Resources:** as many vCPUs as the plan allows. The boot log prints what the
  service actually got (`cgroup CPU quota`).

Variables:

| variable | value |
|---|---|
| `RAILWAY_DOCKERFILE_PATH` | `Dockerfile.worker` |
| `PORT` | `8080`. Set it explicitly so the backend can reference it. |
| `WORKER_TOKEN` | `openssl rand -hex 32` |
| `R2_ENDPOINT` | `https://<account-id>.r2.cloudflarestorage.com` |
| `R2_BUCKET` | the bucket PocketBase stores uploads in |
| `R2_ACCESS_KEY_ID` / `R2_SECRET_ACCESS_KEY` | an R2 API token with **Object Read** on that bucket only |
| `WORKER_MAX_JOBS` | optional, default 4 |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | `330`. A redeploy lets running encodes finish: ffmpeg's 5-minute limit plus slack. |

Name the service exactly **`worker`**, because the backend's variables reference
it by that name.

A healthy boot logs the ffmpeg version, `libsvtav1, libx264, libwebp, aac ok`
and `encode worker listening`.

## 3. Backend service

Settings:

- **Source:** this repo (see §4). `Dockerfile` is the default, so it needs no
  path variable.
- **Serverless:** **off**. A sleeping backend would drop the Discord bot.
- **Volume:** attach one at **`/app/pb_data`**.
- **Healthcheck path:** `/api/health`.
- **Networking:** generate a Railway domain on port **8080** for testing. The
  custom domain comes at cutover (§6).

Variables: everything from the Windows host's `.env` (see `.env.example`), plus
the following:

| variable | value |
|---|---|
| `ENCODE_WORKER_URL` | `http://${{worker.RAILWAY_PRIVATE_DOMAIN}}:${{worker.PORT}}` |
| `ENCODE_WORKER_TOKEN` | `${{worker.WORKER_TOKEN}}` |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | `600` |
| `DISCORD_BOT_ENABLED` | `false` **until cutover**. Two bots on one token would ingest everything twice. |
| `MAX_PROCESS_JOBS` | `2`. An upload's animated WebP preview encodes on about one core, so with one upload at a time most of the worker sits idle during that step. With 2, the next upload's AV1 encode uses those idle cores. Its two MP4 encodes run in parallel, so 2 uploads fill the worker's 4 job slots (`WORKER_MAX_JOBS`). |

Don't set `GOYANGI_SUPERVISED`, `GOYANGI_H264_ENCODER` or `GOYANGI_VAAPI_DEVICE`.
They belong to the Windows host.

**Deploys and work in flight.** A Railway redeploy sends SIGTERM. The server
then stops taking uploads (they get a 503 "Server is updating" response) and
waits for encodes, converts, R2 deletes and bot work to finish. It shuts down
when that work is done, or 15 s before the draining time runs out
(`hooks/deploy.go`). If work is cut off, a "Deploy cut off work in flight"
warning goes to `system_logs`. The post can be retried from the site, or it is
re-queued at a later boot once it is more than 10 minutes old. A service with a
volume can't overlap deployments, so the API is down from the end of the drain
until the new deployment is up.

At boot, preflight wakes the worker and logs `encode worker up in …`. If you
see `encode worker unreachable` instead, check the two `ENCODE_WORKER_*`
variables and the worker's name.

## 4. Deploying code

There are two ways to deploy:

- **From your machine:** `railway link`, then `railway up` from the repo root,
  once per service (`railway up --service backend`, `--service worker`). This
  uploads the working tree. Use it while this work is still on `exp`.
- **From GitHub:** connect the deploy repo to both services, on `exp` until
  it's merged and then on `main`. Every push to that branch deploys both
  services, which replaces the Windows updater.

A GitHub deploy stamps the commit into the binary, and boot logs it to
`system_logs`. A `railway up` deploy reports `dev`.

## 5. Move the data

The database moves as a PocketBase backup.

1. **Before the day:** deploy both services as above. The backend boots on an
   empty volume with the bot off. Create a superuser on it:
   `railway ssh --service backend`, then
   `/app/myapp superuser upsert <email> <password>`.
2. **Stop the Windows host** (the host runs these, as admin):
   `nssm stop goyangi`. That stops the bot and freezes the database. Downtime
   starts here.
3. **Zip the database on the Windows host.** Zip the **contents** of
   `C:\goyangi\pb_data`, not the folder itself, and leave out `backups`. Name the
   zip in lowercase letters, digits, `-` and `_`, for example
   `pb_data_move.zip`. The host sends it to you privately. It holds user
   accounts.
4. **Restore it on Railway.** Open the backend's Railway domain `/_/`, go to
   Settings → Backups → **Upload backup**, then **Restore**. PocketBase replaces
   `pb_data` and restarts. The superuser from step 1 is replaced by the old
   one, so log in with that.
5. **Check it on the Railway domain.** Log in, browse, open a post, upload a
   test clip and watch it encode (the `📊 encode[remote]` lines). PocketBase's
   S3 settings move with the database, so files keep coming from R2.

## 6. Cutover

1. **Turn the bot on.** Remove `DISCORD_BOT_ENABLED` from the backend (or set it
   to `true`). Changing a variable redeploys the service.
2. **Remove the tunnel route.** In Cloudflare Zero Trust → Networks →
   Tunnels → the tunnel → Public hostnames, delete `api.goyangi.pics`.
3. **Add the custom domain.** On the backend, add `api.goyangi.pics` with port
   **8080**. Railway shows the DNS records it needs, a CNAME and a TXT for
   verification. Add them in Cloudflare DNS. With the record proxied (orange
   cloud), Cloudflare's SSL/TLS mode must be **Full**. Follow Railway's prompt
   for the current details.
4. **Check the live site.** The frontend already calls `api.goyangi.pics`, so it
   needs no change.

**Rollback:** until the Windows host is cleared, run `nssm start goyangi` there
and put the tunnel route back. Anything written on Railway after the cutover
stays behind on Railway.

## 7. After the move

- **Backups:** in PocketBase, go to Settings → Backups. Turn on scheduled
  backups and store them in R2 (S3). Otherwise they fill the volume.
- **Retire the Windows host** after a few quiet days. The host runs
  `nssm remove goyangi confirm`, deletes the `cloudflared` service and deletes
  `C:\goyangi`, including `pb_data` (personal data), and the tunnel is deleted
  in Cloudflare.
- **Privacy records:** PocketBase is no longer self-hosted. Railway, a US
  company, becomes a processor for account data and server logs. Update
  `docs/ropa.md` (rows 1, 4 and 6: processor, transfers, journald note) and the
  frontend's privacy page to match.
- **Repo cleanup:** `scripts/updater`, `docs/self-host-windows.md` and the
  supervised-drain path in `hooks/deploy.go` only serve the Windows host.
