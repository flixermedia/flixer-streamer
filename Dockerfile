FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/flixer-streamer ./cmd/flixer-streamer

FROM alpine:3.22 AS runtime
RUN apk add --no-cache ca-certificates curl ffmpeg \
    && adduser -D -H -u 65532 streamer \
    && mkdir -p /var/cache/flixer/transcode \
    && chown -R streamer:streamer /var/cache/flixer

COPY --from=build /out/flixer-streamer /usr/local/bin/flixer-streamer

ENV PORT=8091 \
    FFMPEG_PATH=ffmpeg \
    FFPROBE_PATH=ffprobe \
    TRANSCODE_DIR=/var/cache/flixer/transcode

USER streamer
EXPOSE 8091

HEALTHCHECK --interval=15s --timeout=5s --start-period=10s --retries=5 \
    CMD curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/flixer-streamer"]
