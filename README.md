# AI_README.md: Flixer-Streamer Autonomous Generation Specification

> **AI Context & Directive:** You are building `flixer-streamer`, a high-throughput, low-memory Go microservice for media streaming. Adhere strictly to the package layout, interface contracts, and error-handling patterns defined in this document. Do not invent custom frameworks; stick to Go standard libraries and battle-tested packages.

---

## 1. Architectural Blueprint & Data Flow

[flixer-web] ---> (1. Request Play URL) ---> [flixer (Core API)]
                                                   |
                                           (2. Returns Signed Token 
                                            & Stream URL)
                                                   v
[flixer-web] ---> (3. GET /stream?file=...&token=...) ---> [flixer-streamer]
                                                                  |
                                                    (4. Validate HMAC/JWT)
                                                                  |
                                                    (5. Serve Range / Chunk)

---

## 2. Directory Structure & File Contracts

The codebase must follow this exact layout:

flixer-streamer/
├── cmd/
│   └── server/
│       └── main.go           # Wires config, storage, middleware, and HTTP router
├── internal/
│   ├── config/
│   │   └── config.go         # Env var parser (PORT, STORAGE_TYPE, MEDIA_ROOT, JWT_SECRET)
│   ├── handler/
│   │   ├── stream.go         # HTTP handler implementing http.ServeContent & Range logic
│   │   └── health.go         # GET /health returning JSON status
│   ├── middleware/
│   │   ├── auth.go           # HMAC-SHA256 or JWT validation middleware extracting claims
│   │   └── logging.go        # Structured logging (log/slog) for incoming requests
│   └── storage/
│       ├── storage.go        # Interface definition: StorageDriver
│       ├── local.go          # Local POSIX filesystem implementation of StorageDriver
│       └── s3.go             # AWS S3 / MinIO implementation of StorageDriver
├── go.mod
└── go.sum

---

## 3. Core Interface Specifications (AI Code Generation Contracts)

### 3.1 Storage Driver Interface (internal/storage/storage.go)
Any storage backend must implement this exact interface to allow seamless switching between local disk and object storage.

```go
package storage

import (
	"context"
	"io"
	"os"
)

type StorageDriver interface {
	// Open returns a read seeker and file info for the requested asset path
	Open(ctx context.Context, path string) (io.ReadSeekCloser, os.FileInfo, error)
	// Exists checks if a media asset is present
	Exists(ctx context.Context, path string) (bool, error)
}
