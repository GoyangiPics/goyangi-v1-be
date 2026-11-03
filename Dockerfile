# Backend (PocketBase + the Discord bot) — the Railway "backend" service; see
# docs/railway-hosting.md. The encode worker builds from Dockerfile.worker.

# --- Build stage ---
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy dependency files first (for better caching)
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Railway passes the deployed commit to builds that declare it; the server logs
# it to system_logs at boot. Empty for `railway up`, which reports "dev".
ARG RAILWAY_GIT_COMMIT_SHA
RUN CGO_ENABLED=0 go build -ldflags "-X main.commit=${RAILWAY_GIT_COMMIT_SHA:-dev}" -o myapp .

# --- Run stage ---
FROM debian:trixie-slim

# FFmpeg 9.0, the same build as Dockerfile.worker — keep the two in step. With
# the encode worker on, this server only runs ffprobe, but a local encode (the
# worker unset) then matches the worker's output. ca-certificates for Discord
# and R2 over HTTPS.
ARG FFMPEG_BUILD=ffmpeg-n9.0-latest-linux64-gpl-9.0
ARG FFMPEG_RELEASE=https://github.com/BtbN/FFmpeg-Builds/releases/download/latest
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl xz-utils \
 && cd /tmp \
 && curl -fsSLO "$FFMPEG_RELEASE/$FFMPEG_BUILD.tar.xz" \
 && curl -fsSL "$FFMPEG_RELEASE/checksums.sha256" | grep "  $FFMPEG_BUILD.tar.xz\$" | sha256sum -c - \
 && tar -xJf "$FFMPEG_BUILD.tar.xz" \
 && mv "$FFMPEG_BUILD/bin/ffmpeg" "$FFMPEG_BUILD/bin/ffprobe" /usr/local/bin/ \
 && rm -rf /tmp/* \
 && apt-get purge -y curl xz-utils \
 && apt-get autoremove -y \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /app/myapp /app/myapp

# The Railway volume mounts here; PocketBase keeps pb_data next to the binary.
RUN mkdir /app/pb_data

EXPOSE 8080

# 0.0.0.0 is required for Railway to map the port correctly
CMD ["/app/myapp", "serve", "--http=0.0.0.0:8080"]
