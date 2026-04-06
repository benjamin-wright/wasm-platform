# execution-host

The runtime engine of the wasm platform: receives per-application configuration from the wp-operator, loads WASM modules from the module cache, subscribes to NATS subjects, and invokes modules with scoped host functions.

## Configuration Sync

On startup (or desync) the execution host requests a full configuration snapshot from the wp-operator via gRPC. It then maintains a bidirectional stream for incremental deltas. See [`proto/configsync/v1/configsync.proto`](../../proto/configsync/v1/configsync.proto) for the schema. Rust stubs are generated at build time via `build.rs` / `tonic-build`.

## Module Loading

When a new or updated application config arrives, the execution host:

1. Checks the module cache for a precompiled `.cwasm` keyed by `(digest, arch, wasmtime_version)`.
2. On hit, loads directly. On miss, pulls the raw `.wasm` OCI artifact, AOT-compiles it, pushes the result back to the cache, then loads it.

## NATS Message Handling

The execution host connects to the shared NATS instance using credentials read from files at `NATS_CREDENTIALS_PATH` (a Kubernetes secret volume mount). Credentials are re-read on every connection attempt, so rotations are picked up without pod restarts.

It subscribes to per-application subjects (the fully-prefixed topics pushed by the operator). On each message:

1. **`message-application`** — payload is passed to the module's `on-message` export. If the NATS message has a reply subject, the response bytes are published back.
2. **`http-application`** — the platform-private JSON payload is decoded into typed WIT records, passed to `on-request`, and the returned `HttpResponse` is serialised back to JSON for the NATS reply.

Concurrency is bounded by a semaphore (default 64, configurable via `MAX_CONCURRENT_INVOCATIONS`).

## Data Isolation

- **PostgreSQL** — per-app connection pools keyed by `(database_name, username)`, lazily initialized. Connection strings are built from the shared `PG_HOST`/`PG_PORT` combined with per-app credentials from `SqlConfig` in the config stream.
- **Redis** — single multiplexed connection to the shared instance. Keys are transparently prefixed with `<namespace>/<spec.keyValue>/`. Apps sharing a `spec.keyValue` within the same namespace intentionally share key-space.
- **NATS** — each app is bound to its own subject. The `messaging` host function publishes only to the app's declared scope.

## Environment Variables

| Variable | Description |
|---|---|
| `CONFIG_SYNC_ADDR` | gRPC address of the wp-operator (required). |
| `MODULE_CACHE_ADDR` | HTTP address of the module cache (required). |
| `NATS_CREDENTIALS_PATH` | Directory containing NATS credential files (required). |
| `PG_HOST` | Shared PostgreSQL hostname. |
| `PG_PORT` | Shared PostgreSQL port. |
| `REDIS_URL` | Shared Redis URL (e.g. `redis://redis:6379`). |
| `MAX_CONCURRENT_INVOCATIONS` | Concurrency limit per host (default `64`). |
| `HOSTNAME` | Used as `host_id` in gRPC (injected by downward API). |