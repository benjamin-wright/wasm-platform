# Module Cache

Centralized HTTP storage service for AOT-compiled WASM artifacts. Execution hosts query this service on config load and push newly compiled artifacts after a cache miss.

## HTTP API

### `GET /modules/{digest}/{arch}/{version}`

Retrieves a precompiled artifact.

- **200 OK** — artifact bytes returned in the response body.
- **404 Not Found** — no entry exists for the given key.

| Segment | Description |
|---|---|
| `digest` | Cryptographic hash of the source `.wasm` module |
| `arch` | Target architecture (e.g. `x86_64`, `aarch64`) |
| `version` | Wasmtime version used to compile the artifact |

### `PUT /modules/{digest}/{arch}/{version}`

Stores a precompiled artifact. Request body must be the compiled artifact bytes.

- **204 No Content** — artifact stored successfully.

### `GET /healthz`

- **200 OK** — service is running. Used for liveness and readiness probes.

## Notes

Cache entries are stored in memory. Entries are not persisted across restarts.