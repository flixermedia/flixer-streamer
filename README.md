You are an expert systems engineer building the streaming layer for Flixer, an open-source self-hosted media server.

CONTEXT – EXISTING CODEBASE
- Backend (core API + auth + library metadata): https://github.com/flixermedia/flixer
  - TypeScript, Ts.ED + Fastify, TypeORM + PostgreSQL, pg-boss jobs, bearer-token / session auth.
  - Early stage: media scanner and transcoding engine are still on the roadmap.
  - Settings live in a global `settings` table (JSON values). Auth is already implemented (register/login/sessions).
- Frontend: https://github.com/flixermedia/flixer-web (Vue 3 + Vite + Pinia).
- Streaming service (new, to be built/expanded): https://github.com/flixermedia/flixer-streamer
  - Currently sketched as a high-throughput Go microservice.
  - Intended flow (from its README):
    1. Client asks flixer backend for a play URL.
    2. Backend returns a signed token + stream URL pointing at flixer-streamer.
    3. Client hits flixer-streamer with the token.
    4. Streamer validates the token and serves the media (Range / chunks).

GOAL
Design and implement a production-ready streaming protocol for Flixer that is conceptually similar to Jellyfin’s (HLS + progressive HTTP with Range support, Direct Play / Direct Stream / Transcode decisioning, adaptive bitrate when needed) while fitting Flixer’s split architecture:

- flixer (Node/Ts.ED) owns: library metadata, user accounts, authorization, play-session creation, and the “what should I stream?” decision.
- flixer-streamer (Go preferred, or Node if you justify it) owns: high-performance media delivery, Range requests, optional on-the-fly transcoding/HLS segmentation, and multiple media roots.

REQUIREMENTS THAT MUST BE MET
1. Authorization
   - Every stream request must be authorized by the flixer backend.
   - Preferred pattern: short-lived signed JWT or HMAC token issued by flixer that contains:
     - userId, mediaItemId (or path/hash), mediaSourceId, allowed bitrates/codecs, expiry, playSessionId, optional startTimeTicks, audio/subtitle indices.
   - flixer-streamer validates the token (shared secret from .env) and rejects expired / forged / mismatched requests. Do not trust the client with filesystem paths.

2. Media locations – multiple folders
   - flixer-streamer must support multiple media roots configured via .env (or a config file loaded from .env).
   - Example:
     MEDIA_ROOTS=/media/movies:/media/tv:/media/anime:/mnt/nas/music
     or JSON array MEDIA_ROOTS=["/media/movies","/media/tv",...]
   - Paths returned by the backend must be resolved against these roots safely (prevent path traversal). Support both local filesystem and (optionally later) S3/MinIO via a StorageDriver interface.

3. Streaming modes (mirror Jellyfin’s decision tree)
   - Direct Play: serve the original file with HTTP Range requests (Content-Range, Accept-Ranges). Preferred whenever the client can play the container + codecs.
   - Direct Stream / Remux: change container only (e.g. MKV → MP4/TS) or copy streams; still mostly zero-CPU.
   - Transcode: full or partial re-encode when codecs, resolution, bitrate, or subtitles are incompatible. Use FFmpeg (jellyfin-ffmpeg style capabilities preferred).
   - HLS (primary adaptive protocol):
     - Master playlist (.m3u8) listing quality variants.
     - Media playlists listing segments (.ts or fMP4).
     - Segment duration default ~6 s (configurable). Support VOD playlists.
     - Optional adaptive bitrate variants generated on the fly or from pre-encoded renditions.
   - Progressive HTTP download with Range must always work for Direct Play.

4. Playback negotiation (Jellyfin-style PlaybackInfo)
   - Client (or backend on behalf of client) calls an endpoint on flixer, e.g. POST /api/items/{id}/playback-info (or /stream/info).
   - Request body contains a DeviceProfile (supported containers, video/audio codecs, max bitrate, max resolution, subtitle methods, etc.).
   - Backend returns MediaSources with:
     - supportsDirectPlay / supportsDirectStream / supportsTranscoding
     - Path or stream URL (pointing at flixer-streamer)
     - TranscodingUrl when needed
     - PlaySessionId
     - Available audio/subtitle streams
   - Backend issues the signed token that the streamer will later validate.

5. Communication between services
   - flixer-streamer never talks to the database. It only validates tokens and serves files / runs FFmpeg.
   - Optional: streamer can call a lightweight internal endpoint on flixer to re-validate a play session or report progress, but keep it optional and non-blocking.
   - Shared secrets and configuration via .env on both sides (JWT_SECRET / STREAM_TOKEN_SECRET, MEDIA_ROOTS, FFMPEG_PATH, TRANSCODE_CACHE_DIR, etc.).

6. Performance & reliability
   - Efficient Range support (http.ServeContent style or equivalent).
   - Transcoding jobs must be cancellable when the client stops (use playSessionId).
   - Segment cache / temporary files cleaned up after inactivity.
   - Low memory footprint; prefer streaming FFmpeg output rather than writing entire files when possible.
   - Health endpoint, structured logging, metrics-ready.

7. Configuration (.env)
   Required variables for flixer-streamer:
   - PORT
   - JWT_SECRET or STREAM_TOKEN_SECRET (must match flixer)
   - MEDIA_ROOTS (multiple folders, colon- or JSON-separated)
   - FFMPEG_PATH (optional, default “ffmpeg”)
   - TRANSCODE_DIR or CACHE_DIR
   - ALLOWED_ORIGINS / CORS if needed
   - LOG_LEVEL
   Optional: hardware acceleration flags (NVENC, QSV, VAAPI, etc.), max concurrent transcodes, segment length.

8. Security
   - No direct filesystem path exposure to clients.
   - Token expiry short (e.g. 1–4 hours, refreshable).
   - Path sanitization against the configured MEDIA_ROOTS.
   - Rate limiting / concurrent stream limits per user (can be enforced by token claims or by backend).

DELIVERABLES YOU MUST PRODUCE
1. Detailed design document (architecture, sequence diagrams in text, data structures for tokens and DeviceProfile, HLS playlist examples).
2. Concrete API contracts:
   - Backend endpoints (playback-info, start/stop play session, progress reporting).
   - Streamer endpoints (e.g. GET /stream/{token}, GET /hls/{session}/master.m3u8, segment routes, progressive Range routes).
3. Implementation plan for flixer-streamer (Go preferred – follow the existing internal/ package layout if present; otherwise propose a clean layout).
4. Token format (JWT claims) and validation logic.
5. How the backend decides Direct Play vs Transcode (simplified DeviceProfile matching).
6. FFmpeg command templates for common cases (H.264/AAC HLS, remux to fMP4/TS, subtitle burn-in, audio downmix).
7. .env.example for both services.
8. Integration notes for the Vue frontend (how to obtain a play URL and feed it to a video player that supports HLS + progressive).
9. Future extension points: hardware acceleration, pre-transcoded variants, Live TV, subtitle extraction, multi-version media.

CONSTRAINTS & STYLE
- Keep the streaming service focused and fast; do not re-implement the whole media library inside the streamer.
- Prefer standard, widely-supported protocols (HLS + HTTP Range) so any modern browser / media player works.
- Code should be production-quality: clear errors, context cancellation, graceful shutdown, no path traversal.
- Document every assumption and every place where Flixer currently differs from full Jellyfin (Flixer is still early-stage).

Start by writing the design document and the exact token + API contracts. Then implement the core path: signed-token progressive Range serving against multiple MEDIA_ROOTS. After that is solid, add HLS master/media playlists and basic on-the-fly transcoding.

Reference mental model (Jellyfin):
- Client → PlaybackInfo (with DeviceProfile) → server decides DirectPlay / DirectStream / Transcode and returns URLs + PlaySessionId.
- For compatible media: plain HTTP Range on the original file.
- For incompatible media: Dynamic HLS (master.m3u8 → variant playlists → .ts/.m4s segments) generated by FFmpeg on demand.
- Everything authenticated; segments and playlists carry the session/token.
