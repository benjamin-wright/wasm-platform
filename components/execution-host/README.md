# execution-host

The runtime engine of the wasm platform: subscribes to NATS messages and invokes WASM modules based on its current configuration.

## Configuration Sync

The execution host syncs configuration with the wp-operator over gRPC (see [`proto/configsync/v1/configsync.proto`](../../proto/configsync/v1/configsync.proto)):

1. **On startup (or desync)** — requests the full current configuration snapshot.
2. **Ongoing** — maintains an open bidirectional stream to receive incremental configuration deltas and acknowledge each one. On failure to apply a delta, falls back to requesting the full configuration again.

Each application config includes the per-application database credentials provisioned by the wp-operator, so the execution host can connect directly to the shared PostgreSQL instance on behalf of each module.

Rust gRPC stubs are generated at build time via `build.rs` using `tonic-build` and included with `tonic::include_proto!("configsync.v1")`.

## NATS Message Handling

The execution host connects to the shared NATS instance using credentials provisioned by the db-operator (via the `NatsAccount` CRD in the platform Helm chart). On each incoming NATS message:

1. The payload is passed to the WASM module's `on-message` export.
2. If the message includes a NATS reply subject, the response bytes returned by the module are published to that subject.

The NATS connection is configured via environment variables injected from the db-operator-managed secret:

| Variable | Description |
|---|---|
| `NATS_URL` | Full NATS URL, e.g. `nats://user:pass@host:4222`. Composed from the individual `NATS_USERNAME`, `NATS_PASSWORD`, `NATS_HOST`, `NATS_PORT` vars that the db-operator populates in the secret. |
| `NATS_TOPIC` | NATS subject to subscribe to. Set in the Helm chart values (`nats.topic`). |

A minimal HTTP server continues to run on port 3000 exclusively to serve `/healthz` for Kubernetes liveness and readiness probes.

## Module Loading

When the execution host receives a new or updated application config (either from a full config response or an incremental update), it:

1. Queries the module cache for a precompiled artifact keyed by module digest, CPU architecture, and Wasmtime version.
2. If found, loads the cached `.cwasm` artifact directly (no compilation required).
3. If not found, pulls the raw `.wasm` OCI artifact from the registry, AOT-compiles it using the local Wasmtime engine, pushes the compiled artifact back to the module cache, and then loads it.

## Data Isolation

The execution host enforces per-application data isolation for the shared Redis and NATS instances:

- **Redis** — every key read or written for an application is prefixed with `<namespace>/<spec.keyValue>/`. The module sees unqualified keys; the host transparently applies the prefix on all Redis operations. Applications in the same namespace that share a `spec.keyValue` value intentionally share key-space.
- **NATS** — each application subscribes and publishes only to the subject declared in its config (`spec.topic`). The execution host binds each module instance to its own NATS subject.

## TODO

1. Explicitly pull in the hello-world WASM module and hard-code the request handler to call it as a POC test.