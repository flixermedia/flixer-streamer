# Flixer streaming design and integration contract

## Status and architectural boundary

This repository contains the Go streaming service: HS256 token validation,
encrypted media locators, configured local media roots, a health check,
progressive HTTP delivery with byte ranges, and ffmpeg-based remuxing and
resolution changes. It does not issue playback tokens,
read Flixer's database, negotiate a playback profile, or create play sessions.
HLS is the next implementation phase.

The Flixer backend currently owns authentication and application APIs; the Vue
client owns playback UI. The backend source currently has no media catalog or
playback-info endpoint, so the API below is a proposed integration contract,
not an already available endpoint. The streamer intentionally does not call
Flixer or connect to its database.

## Request flow

```text
Vue client          Flixer API                 Flixer Streamer        Media roots
    |                    |                            |                    |
    | POST playback-info |                            |                    |
    |------------------->| authorize user/item        |                    |
    |                    | choose source and mode     |                    |
    |                    | sign short-lived token    |                    |
    |<-------------------| URL + profile + session    |                    |
    | GET /stream/{token}                             |                    |
    |----------------------------------------------->| verify JWT          |
    |                                                | decrypt locator     |
    |                                                | resolve under root  |
    |                                                |------------------->|
    |<------------------- 200 / 206 + media bytes ---|                    |
    | POST progress/stop |                            |                    |
    |------------------->| persist session state      |                    |
```

The client never supplies a filesystem path. The only location in a token is a
root ID plus a root-relative path encrypted with AES-GCM. The streamer checks
that the resolved file remains within the configured root, including after
following symlinks. Media roots must be writable only by trusted administrators;
the standard-library implementation cannot prevent an administrator racing a
symlink replacement between path validation and file open.

## Streamer HTTP contract (implemented)

### `GET` or `HEAD /stream/{token}`

`token` is a compact JWT. Client-supplied paths are ignored. Optional query
parameters: `height` (`240`, `360`, `480`, `720` or `1080`; maximum output
height, never upscales) and `start` (seconds to begin from). Anything else is
rejected with `400 invalid_options`.

The streamer picks a delivery mode per request (probing with `ffprobe`):

| Mode | When | Result |
|------|------|--------|
| `DirectPlay` | `.mp4`/`.m4v`/`.webm`, copyable codec, no `height`/`start` | Original file via `http.ServeContent`: `200`, or `206` with `Content-Range` for ranges |
| `DirectStream` | Other containers (e.g. MKV) or `start` set, H.264/VP8/VP9/AV1 video | ffmpeg `-c copy` into fragmented MP4 (H.264) or WebM (others); audio copied if browser-safe, else AAC/Opus |
| `Transcode` | `height` smaller than the source, or an unsupported video codec | ffmpeg re-encode to H.264 (`libx264`, `veryfast`, CRF 23) + AAC in fragmented MP4 |

Remuxed and transcoded responses are live pipes: `200` with no
`Content-Length`, no byte-range support, and an `X-Flixer-Playback-Mode`
header. Clients seek by requesting a new URL with `start`. ffmpeg is killed when
the client disconnects. By default there is no limit; set `MAX_TRANSCODES` to cap
concurrent runs (excess requests wait 10 seconds, then get `503`);
extra requests get `503 transcoder_busy` with `Retry-After`. If ffmpeg is not
installed the streamer logs a warning and serves direct play only (`501` when
`height`/`start` is requested, and for non-direct-playable files `DirectPlay`
is attempted as before).

- Invalid, expired, or forged token: `401` with
  `{"error":"invalid_token","message":"A valid stream token is required"}`
- Missing or disallowed media locator: `404` with `media_not_found`
- Invalid method: `405`; other route: `404`
- Successful bodies are `private, no-store`; requests never log the token or
  raw URL.

### `GET /info/{token}[?height=&start=]`

Same token and options as `/stream`. Returns the mode the stream would use and
the source details, so a client can show quality choices and a seek bar:

```json
{
  "playMethod": "DirectStream",
  "source": {"videoCodec":"h264","width":1920,"height":1080,"durationSeconds":10},
  "availableHeights": [240, 360, 480, 720]
}
```

### `GET /healthz`

Returns `200 {"status":"ok"}`. It is a process liveness check, not a readiness
check for a future transcoder or external dependencies.

Origins are denied if `ALLOWED_ORIGINS` is set and the browser supplies an
unlisted `Origin`. CORS is not authentication; the stream token is mandatory.

## Token contract (HS256 JWT)

All timestamps are integer Unix seconds. The issuer must sign the exact compact
JWT input (`base64url(header) + "." + base64url(payload)`) with HMAC-SHA256 using
the shared `STREAM_TOKEN_SECRET` bytes. Base64url is unpadded. Header:

```json
{"alg":"HS256","typ":"JWT"}
```

Required payload:

```json
{
  "iss": "flixer",
  "aud": "flixer-streamer",
  "sub": "user-id",
  "userId": "user-id",
  "mediaItemId": "catalog-item-id",
  "mediaSourceId": "catalog-source-id",
  "playSessionId": "play-session-id",
  "mediaLocator": "base64url(nonce || AES-256-GCM(ciphertext+tag))",
  "iat": 1800000000,
  "exp": 1800003600,
  "jti": "unique-token-id"
}
```

Optional claims are `nbf`, `startTimeTicks`, `audioStreamIndex`,
`subtitleStreamIndex`, `allowedBitrates` (integer bps), `allowedVideoCodecs`,
and `allowedAudioCodecs`. `sub` must equal `userId`. Tokens must expire within
four hours of `iat`; the validator allows 30 seconds of clock skew for future
`iat`/`nbf`, but an expired token is rejected. Rotate/refresh playback by
obtaining a new token from Flixer.

The plaintext locator is compact JSON:

```json
{"rootId":"root-1","relativePath":"Movies/Example/movie.mp4"}
```

Encrypt its UTF-8 bytes with AES-256-GCM. Derive the 32-byte AES key as
`SHA256(UTF8("flixer-streamer:media-locator:v1") || 0x00 || secretBytes)`, use
the exact string `flixer-streamer:media-locator:v1` as GCM additional
authenticated data, and prepend the 12-byte random nonce to ciphertext plus
the 16-byte GCM tag before unpadded base64url encoding. This hides root-relative
names from clients inspecting the signed token. `rootId` is an opaque ID that
Flixer assigns to each root media folder; the streamer learns the ID-to-path
mapping from Flixer.

The streamer verifies algorithm/type, HMAC, issuer, audience, required claims,
timestamps, maximum token lifetime, and authenticated locator encryption before
opening any file. Use a cryptographically random secret of at least 32 bytes.
Never reuse the secret for unrelated signing purposes.

## Proposed Flixer API contracts (not implemented here)

### `POST /api/items/{itemId}/playback-info`

Authenticated bearer session required. Flixer must authorize access to the item,
choose a source, create a play session, decide playback mode, then sign the
streamer token. Example request:

```json
{
  "deviceProfile": {
    "containers": ["mp4", "webm"],
    "videoCodecs": ["h264", "vp9"],
    "audioCodecs": ["aac", "opus"],
    "subtitleMethods": ["external", "embedded"],
    "maxBitrate": 12000000,
    "maxWidth": 1920,
    "maxHeight": 1080
  },
  "startTimeTicks": 0
}
```

Example response:

```json
{
  "playSessionId": "session-id",
  "playMethod": "DirectPlay",
  "mediaSources": [{
    "mediaSourceId": "source-id",
    "supportsDirectPlay": true,
    "supportsDirectStream": false,
    "supportsTranscoding": false,
    "streamUrl": "https://streamer.example/stream/{signed-token}",
    "transcodingUrl": null,
    "container": "mp4",
    "bitrate": 4200000,
    "streams": [
      {"index": 0, "type": "video", "codec": "h264"},
      {"index": 1, "type": "audio", "codec": "aac", "language": "en"}
    ]
  }]
}
```

Do not return absolute media paths, root paths, or the decrypted `mediaLocator`.
Use `Cache-Control: no-store` for responses containing tokens.

### `POST /api/play-sessions/{playSessionId}/progress`

Authenticated session required. Body: `{"positionTicks":123,"isPaused":false}`.
The backend verifies ownership and updates watch progress; progress reporting is
client-to-backend and is not required for streaming bytes.

### `POST /api/play-sessions/{playSessionId}/stop`

Authenticated session required. Body may include final `positionTicks`; backend
verifies ownership and closes the session. Future HLS jobs should also be
cancelled by the session ID.

## Playback decision (initial backend algorithm)

`DeviceProfile` should use normalized lowercase container and codec names and
include max bitrate/resolution and subtitle methods. For each authorized source:

1. **Direct Play** when its container, video codec, audio codec, selected
   subtitle method, resolution, and bitrate are supported by the profile.
2. **Direct Stream / remux** when all selected streams are supported but the
   container is not. Remux without re-encoding where the output container
   supports those codecs.
3. **Transcode** when a selected codec, bitrate, resolution, or subtitle mode
   is unsupported and an available FFmpeg path can satisfy the profile.
4. Reject with a typed unsupported-media response if no valid output can be
   produced. Never silently claim transcoding support before FFmpeg is wired.

This is intentionally simpler than Jellyfin: Flixer does not currently expose
media stream metadata or a server-side playback-info API. Those catalog and
decision responsibilities belong in Flixer, not this streamer.

## Vue client integration

Once Flixer implements the proposed endpoint, the web client should:

1. Call `POST /api/items/{itemId}/playback-info` through its existing
   authenticated API client, supplying a device profile and optional resume
   position.
2. Use the returned `streamUrl` directly as the `<video>` source. The
   progressive endpoint supports browser-generated `Range` requests; no custom
   range-fetch loop is needed.
3. For a future HLS response, use native HLS where supported and `hls.js` where
   Media Source Extensions are available. Keep the tokenized URL in memory,
   avoid persisting it, and request fresh playback info after expiry.
4. Send periodic progress and stop requests to Flixer, never to the streamer.

Because a media element cannot attach the web app's bearer header to ordinary
source requests, the streamer token is intentionally part of the returned
stream URL. HTTPS, short expiry, no-store responses, and token-redacting
streamer logs are therefore required in deployment.

## HLS and FFmpeg follow-on design (not implemented)

HLS route shape:

```text
GET /hls/{playSessionId}/master.m3u8?token={signed-token}
GET /hls/{playSessionId}/{variant}/index.m3u8?token={signed-token}
GET /hls/{playSessionId}/{variant}/{segment}.ts?token={signed-token}
```

Every route must validate the token and ensure its play-session ID matches the
path. Playlist and segment URLs should carry the same short-lived token; do not
log query strings. Master playlist example:

```m3u8
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-STREAM-INF:BANDWIDTH=2800000,RESOLUTION=1280x720,CODECS="avc1.64001f,mp4a.40.2"
720p/index.m3u8?token=...
#EXT-X-STREAM-INF:BANDWIDTH=1100000,RESOLUTION=640x360,CODECS="avc1.4d401e,mp4a.40.2"
360p/index.m3u8?token=...
```

VOD media playlist example:

```m3u8
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:6
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:6.000,
segment-00000.ts?token=...
#EXTINF:4.200,
segment-00001.ts?token=...
#EXT-X-ENDLIST
```

Initial FFmpeg argument templates (pass as argument arrays, never through a
shell; input paths come only from the validated resolver):

```text
# H.264/AAC HLS, six-second MPEG-TS segments
ffmpeg -i INPUT -c:v libx264 -preset veryfast -c:a aac -b:a 192k \
  -f hls -hls_time 6 -hls_playlist_type vod -hls_segment_filename SEGMENT_%05d.ts PLAYLIST.m3u8

# Remux without re-encoding (choose a container compatible with the codecs)
ffmpeg -i INPUT -map 0:v:0 -map 0:a? -c copy -f mpegts OUTPUT.ts

# Burn an authorized subtitle stream into video (index supplied by backend)
ffmpeg -i INPUT -filter_complex "[0:v]subtitles=INPUT:si=SUBTITLE_INDEX[v]" \
  -map "[v]" -map 0:a? -c:v libx264 -c:a aac OUTPUT

# Downmix audio to stereo while transcoding video
ffmpeg -i INPUT -c:v libx264 -c:a aac -ac 2 -b:a 192k OUTPUT
```

Subsequent work should add bounded transcode concurrency, request/session
cancellation, isolated per-session cache directories, inactivity cleanup,
hardware acceleration capability detection (NVENC/QSV/VAAPI), and segment cache
limits. Later extension points include pre-transcoded variants, Live TV,
subtitle extraction, and alternate media versions. Do not expose FFmpeg
arguments or filesystem paths to clients.

## Configuration and rollout

The streamer reads `.env` without overriding variables already in the process
environment. Root media folders are not configured here: the streamer fetches
them from Flixer (`GET /api/streamer/media-roots`, signed with HMAC-SHA256 over
`flixer-streamer:media-roots:v1:<unix seconds>` using `STREAM_TOKEN_SECRET`) and
refreshes them every minute; unusable roots are skipped. `FFMPEG_PATH`
(default `ffmpeg`), `FFPROBE_PATH` (default `ffprobe`, or next to an explicit
`FFMPEG_PATH`) and `MAX_TRANSCODES` control remuxing/transcoding. `TRANSCODE_DIR`
is reserved for the HLS phase.

See [`.env.example`](./.env.example) for streamer configuration and
[`examples/flixer.env.example`](./examples/flixer.env.example) for a proposed
backend template. The latter is documentation only: no Flixer backend code or
configuration has been changed, and Flixer must implement token issuance before
this streamer can be used for real playback.

Build and test with `go test ./...`; run with `go run ./cmd/flixer-streamer`.
The service defaults to port `8091`. Put it behind a TLS-terminating reverse
proxy and restrict network access to the trusted Flixer deployment.

Before production use, add operational rate/concurrent-stream limits and
metrics. The initial slice enforces per-token authorization and provides
structured request logs, but it does not yet enforce per-user quotas.
