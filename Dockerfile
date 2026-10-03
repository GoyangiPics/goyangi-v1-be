# --- Build stage ---
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Copy dependency files first (for better caching)
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o myapp .

# --- Run stage ---
FROM alpine:latest

WORKDIR /app

# ca-certificates for the Discord HTTPS API; ffmpeg brings ffprobe too,
# which is required for probing uploaded videos.
RUN apk add --no-cache ca-certificates ffmpeg

COPY --from=builder /app/myapp /app/myapp

RUN mkdir /app/pb_data

EXPOSE 8080

# 0.0.0.0 is required for Railway to map the port correctly
CMD ["/app/myapp", "serve", "--http=0.0.0.0:8080"]
