# execution-host

The runtime engine of the wasm platform: accepts incoming requests and loads and invokes WASM modules based on its current configuration.

## Configuration Sync

The execution host syncs configuration with the wp-operator over gRPC (see [`proto/configsync/v1/configsync.proto`](../../proto/configsync/v1/configsync.proto)):

1. **On startup (or desync)** — requests the full current configuration snapshot.
2. **Ongoing** — maintains an open bidirectional stream to receive incremental configuration deltas and acknowledge each one. On failure to apply a delta, falls back to requesting the full configuration again.

Each application config includes the per-application database credentials provisioned by the wp-operator, so the execution host can connect directly to the shared PostgreSQL instance on behalf of each module.

Rust gRPC stubs are generated at build time via `build.rs` using `tonic-build` and included with `tonic::include_proto!("configsync.v1")`.

## Module Loading

When the execution host receives a new or updated application config (either from a full config response or an incremental update), it:

1. Queries the module cache for a precompiled artifact keyed by module digest, CPU architecture, and Wasmtime version.
2. If found, loads the cached `.cwasm` artifact directly (no compilation required).
3. If not found, pulls the raw `.wasm` OCI artifact from the registry, AOT-compiles it using the local Wasmtime engine, pushes the compiled artifact back to the module cache, and then loads it.

## Data Isolation

The execution host enforces per-application data isolation for the shared Redis and NATS instances:

- **Redis** — every key read or written for an application is prefixed with the application's `spec.keyValue` prefix. The module sees unqualified keys; the host transparently applies the prefix on all Redis operations. The exact prefix format is still under discussion (see open questions in the project README).
- **NATS** — each application subscribes and publishes only to the subject declared in its config (`spec.topic`). The execution host binds each module instance to its own NATS subject.

## TODO

1. Explicitly pull in the hello-world WASM module and hard-code the request handler to call it as a POC test.