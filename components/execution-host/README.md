# execution-host

The runtime engine of the wasm platform: accepts incoming requests and loads and invokes WASM modules based on its current configuration.

## Configuration Sync

The execution host syncs configuration with the wp-operator over gRPC (see [`proto/configsync/v1/configsync.proto`](../../proto/configsync/v1/configsync.proto)):

1. **On startup (or desync)** — requests the full current configuration snapshot.
2. **Ongoing** — maintains an open bidirectional stream to receive incremental configuration deltas and acknowledge each one. On failure to apply a delta, falls back to requesting the full configuration again.

Rust gRPC stubs are generated at build time via `build.rs` using `tonic-build` and included with `tonic::include_proto!("configsync.v1")`.

## Module Loading

When the execution host receives a new or updated application config (either from a full config response or an incremental update), it:

1. Queries the module cache for a precompiled artifact keyed by module digest, CPU architecture, and Wasmtime version.
2. If found, loads the cached `.cwasm` artifact directly (no compilation required).
3. If not found, pulls the raw `.wasm` OCI artifact from the registry, AOT-compiles it using the local Wasmtime engine, pushes the compiled artifact back to the module cache, and then loads it.

## TODO

1. explicitly pull in the hello-world WASM module and hard-code the request handler to call it as a POC test.