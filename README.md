<div align="center">

<img src="https://raw.githubusercontent.com/flixermedia/flixer/main/resources/logo/flixer-logo.png" alt="Flixer logo" width="112" />

# Flixer Streamer

**Secure, efficient playback for your library.**

[![Stars](https://img.shields.io/github/stars/flixermedia/flixer-streamer?style=flat-square)](https://github.com/flixermedia/flixer-streamer/stargazers) [![Issues](https://img.shields.io/github/issues/flixermedia/flixer-streamer?style=flat-square)](https://github.com/flixermedia/flixer-streamer/issues)

[Flixer](https://github.com/flixermedia/flixer) · [Documentation](https://github.com/flixermedia/flixer-docs) · [Report an issue](https://github.com/flixermedia/flixer-streamer/issues)

</div>

---

> [!WARNING]
> **Under active development.** Flixer components and integration contracts may change. Check current source and release notes before upgrading production installations.

> [!NOTE]
> **Role in Flixer:** The Go service responsible for authorised media delivery and FFmpeg processing. For an end-to-end deployment, start with the [Flixer backend repository](https://github.com/flixermedia/flixer).

## ✨ Features

- **Direct Play:** HTTP range requests for compatible files.
- **Direct Stream:** FFmpeg remuxing to fragmented MP4 or WebM.
- **Transcoding:** H.264/AAC conversion and requested output-height limits.
- **Browser compatibility:** Optional H.264/MP3, VP8/Vorbis, or VP9/Opus output when AAC playback is unavailable.
- **Protected playback:** Signed JWTs and encrypted media locators.
- **IPTV live HLS:** Refreshable playlists and proxied segments with public-address-only upstream validation.
- **Multiple media roots:** Server-controlled locations and filesystem containment checks.
- **Trickplay:** Private delivery of thumbnail sheets produced by the indexer.

## 🏗️ Architecture

Flixer separates application logic from its web UI, media indexing, media delivery, update management, plugins and documentation. **Flixer Streamer** owns the following responsibility: the Go service responsible for authorised media delivery and FFmpeg processing.

For the full platform, the API coordinates services; the browser interacts with the backend and the streamer receives authorised playback requests. Components should be operated as part of the supported Flixer stack unless you are developing or testing them directly.

## 🚀 Getting started

### Recommended: deploy with Flixer
Use the [main installation guide](https://github.com/flixermedia/flixer#-install). The Compose stack supplies the signing secret, trusted media roots and proxy routing.

### Local Go development

Requires Go 1.25+, FFmpeg and FFprobe.

```bash
git clone https://github.com/flixermedia/flixer-streamer.git
cd flixer-streamer
cp .env.example .env
# Configure APP_SECRET and FLIXER_URL
go build -o flixer-streamer ./cmd/flixer-streamer
./flixer-streamer
curl http://localhost:8091/streamer/healthz
```

A passing health check does not issue playback access; `/streamer/stream/{token}` requires a backend-signed token. For containers, use `docker build -t flixer-streamer:local .`.

## ⚙️ Configuration

| Variable | Purpose |
| --- | --- |
| `PORT` | HTTP listener; default `8091`. |
| `APP_SECRET` | Backend-matching signing secret (at least 32 random bytes). |
| `FLIXER_URL` | Backend URL for trusted media root discovery. |
| `FFMPEG_PATH` / `FFPROBE_PATH` | Media executables. |
| `MAX_TRANSCODES` | Concurrent FFmpeg limit; `0` means unlimited. |
| `TRICKPLAY_DIR` | Shared cached thumbnails from the indexer. |
| `ALLOWED_ORIGINS` | Browser CORS allowlist; not a replacement for token security. |

See [`.env.example`](.env.example) for complete options.

### Experimental Flixer Logger export

When Flixer is paired with a logger, the streamer obtains an application-bound
credential using its existing shared application secret. `FLIXER_URL` must be
HTTPS or literal loopback HTTP, unless `FLIXER_LOGGER_CONTROL_SOCKET` selects
Flixer's protected local Unix socket; non-loopback plaintext is deliberately refused.
The logger endpoint itself also requires HTTPS except on literal loopback.
Export uses a bounded asynchronous queue and retries, and does not replace
stdout or Sentry. Streamer no longer writes application-managed log files.
Events can be dropped on overload, outages, configuration changes or shutdown.
Changes are polled every five seconds; until the next refresh, events may still
go to the previous deployment. A shared secret is not a distinct service
identity. Do not rely on redaction to make logging secrets safe. The bundled
Compose stack provisions the logger and enrolls Streamer through the protected
local control socket.

The checked-in `third_party/flixer-logger` snapshot makes independent builds
possible before the logger module is published. Refresh it from the logger
repository with `bash scripts/vendor-go-sdk.sh /path/to/flixer-streamer`.

## 🔌 Endpoints

| Route | Purpose |
| --- | --- |
| `GET /streamer/healthz` | Service health. |
| `GET /streamer/info/{token}` | Playback information. |
| `GET, HEAD /streamer/stream/{token}` | Authorized media delivery; `height`, `start`, `audioTrack`, and `compat` (`mp3`, `vp8`, or `vp9`) options. |
| `GET, HEAD /streamer/hls/{token}/...` | Authorized VOD HLS playlist and segments. |
| `GET /streamer/live/{token}` | Authorized IPTV live HLS playlist; refresh to load current live media. |
| `GET /streamer/live/{token}/resource/{id}` | Opaque, playlist-issued HLS resource proxy. |
| `GET /streamer/lyrics/{token}` | Sidecar lyrics. |
| `GET /streamer/subtitles/{token}` | Sidecar subtitle listing or file. |
| `GET, HEAD /streamer/trickplay/{token}/tiles.m3u8` | Authorized HLS image playlist for hover-preview thumbnails. Playlist tile URLs resolve to `/{width}/{tile}.jpg`. |
| `GET, HEAD /streamer/trickplay/{token}/index.json` | Legacy trickplay metadata endpoint. |
| `GET, HEAD /streamer/trickplay/{token}/{tile}.jpg` | Legacy thumbnail-sheet endpoint. |

When Flixer's generated cache has no trickplay for a video, the streamer can read Jellyscrub's media-side files from the sibling `trickplay/` directory: `<media-basename>-manifest.json` and `<media-basename>-<width>.bif`. It reads the BIF index directly and serves each embedded JPEG through the authenticated HLS image-playlist routes; no conversion or write access to the media library is needed. Flixer's own generated cache takes precedence.
| `GET /streamer/admin/sessions` | Active sessions; requires the derived admin bearer key. |
| `DELETE /streamer/admin/sessions/{id}` | Cancel an active session; requires the derived admin bearer key. |

The backend issues playback tokens; this service validates and serves them. Playback supports Direct Play, remux/transcode, VOD HLS, and signed IPTV live HLS delivery; this is not a standalone library UI. Read [`DESIGN.md`](DESIGN.md) for implementation details and proposed future APIs.

## 🔐 Security

Use HTTPS, private media mounts, restricted filesystem permissions, short-lived signed URLs and a strong matching `APP_SECRET`. Live HLS proxy requests accept only HTTP(S) upstreams on ports 80, 443, 8080, or 8443, resolve and pin public IP addresses, and validate redirects; they cannot access private or local network addresses. Keep the service behind the trusted stack's proxy.

## 🧩 Flixer ecosystem

| Repository | Purpose |
| --- | --- |
| [Flixer](https://github.com/flixermedia/flixer) | The self-hosted backend API, authentication, libraries, and administration service. |
| [Flixer Web](https://github.com/flixermedia/flixer-web) | The Vue 3 application that powers browsing, sign-in, discovery, and playback interfaces. |
| [Flixer Streamer](https://github.com/flixermedia/flixer-streamer) | The Go service responsible for authorised media delivery and FFmpeg processing. |
| [Flixer Indexer](https://github.com/flixermedia/flixer-indexer) | The Go worker and API service for discovering, identifying, probing, and enriching media. |
| [Flixer Updater](https://github.com/flixermedia/flixer-updater) | The Go sidecar that orchestrates Docker Compose updates and restores. |
| [Flixer Plugins](https://github.com/flixermedia/flixer-plugins) | The official versioned catalogue of Flixer plugins and plugin schemas. |
| [Flixer Docs](https://github.com/flixermedia/flixer-docs) | The Docusaurus source for Flixer platform documentation. |

## 🤝 Contributing

Contributions, bug reports and documentation improvements are welcome. Open an [issue](https://github.com/flixermedia/flixer-streamer/issues) with reproduction steps or a focused proposal, then submit a pull request against the repository. Keep configuration, examples and documentation aligned with implemented code. For backend contributions, also see the [main contribution guidelines](https://github.com/flixermedia/flixer/blob/main/CONTRIBUTING.md).

## 📄 Licence

Check this repository's [`LICENSE`](LICENSE) file, if present, for the authoritative licensing terms. The [main Flixer backend](https://github.com/flixermedia/flixer/blob/main/LICENSE) uses MIT; licensing for separate components should be verified individually before redistribution.

---

<div align="center">

**Your media. Your server. Your rules.**

Made for the [Flixer](https://github.com/flixermedia) ecosystem.

</div>
