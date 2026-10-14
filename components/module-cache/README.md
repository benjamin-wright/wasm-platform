# Module Cache

Shared, in-memory storage for verified binary Wasm. Execution hosts verify downloaded bytes and compile locally with their own engine. Entries are immutable and keyed by the SHA-256 digest of the raw Wasm bytes; native compiled artifacts are rejected.

## HTTP API

`GET /modules/{digest}` returns raw Wasm with `Content-Type: application/wasm` (`200`) or a cache miss (`404`). `digest` must be canonical `sha256:` followed by 64 lowercase hexadecimal characters.

`PUT /modules/{digest}` verifies the binary Wasm header and digest before storage (`204`). Repeating the same content is idempotent. Invalid digest syntax returns `400`; non-Wasm or mismatched bytes return `422`. The legacy architecture/version routes are unavailable.

Uploads are limited to 16 MiB, two simultaneous bodies and a five-second body deadline. Oversized requests return `413`, full intake returns `429`, and stalled bodies return `408`. At most two artifact response buffers remain retained at a time; further cache hits return `429` until a response buffer and all its clones are dropped. Response ownership preserves bytes across eviction and transport buffering.

Storage is limited to 64 MiB and 256 entries, with least-recently-used eviction. Upload bodies and retained response buffers each use at most another 32 MiB. Entries are lost on restart.

`GET /healthz` reports liveness (`200`). `GET /readyz` reports readiness (`200`), changing to `503` during shutdown. The service stops admitting artifact work during shutdown and drains accepted HTTP work for up to ten seconds.

`GET /metrics` exposes Prometheus gauges for cached bytes, entries, active uploads and retained response buffers, and counters for hits, misses, stores, rejections and evictions.
