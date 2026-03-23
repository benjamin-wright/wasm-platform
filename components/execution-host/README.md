# execution-host

The runtime engine of the wasm platform: accepts incoming requests and loads and invokes WASM modules based on its current configuration.

## Configuration Sync

The execution host stays in sync with the wp-operator via gRPC (`ConfigSync` service):

1. **On startup (or desync)** — the host calls `RequestFullConfig` to fetch the latest complete configuration from the operator. This gives it the full list of `ApplicationConfig` entries with all env vars, binding references, and resolved module references.
2. **Ongoing updates** — the host calls `PushIncrementalUpdate` and keeps the bidirectional stream open. The operator streams config deltas (`IncrementalUpdateRequest` messages) to the host; the host applies each delta and streams back an `IncrementalUpdateAck`. If an update fails to apply, the host closes the stream and recovers by calling `RequestFullConfig` again.

## Module Loading

When the execution host receives a new or updated application config (either from a full config response or an incremental update), it:

1. Queries the module cache for a precompiled artifact keyed by module digest, CPU architecture, and Wasmtime version.
2. If found, loads the cached `.cwasm` artifact directly (no compilation required).
3. If not found, pulls the raw `.wasm` OCI artifact from the registry, AOT-compiles it using the local Wasmtime engine, pushes the compiled artifact back to the module cache, and then loads it.

## TODO

1. explicitly pull in the hello-world WASM module and hard-code the request handler to call it as a POC test.