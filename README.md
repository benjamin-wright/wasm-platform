# WASM-Platform

A serverless application platform that runs WebAssembly guest modules on Kubernetes. Guest code interacts with SQL databases, key-value stores, and message queues through a strongly-typed WIT interface — the platform handles provisioning, sandboxing, and scaling.

## Current Status — Phase 0 (Proof of Concept)

The project is in its earliest phase: a single Rust binary that loads a `.wasm` guest module and invokes it on incoming NATS messages. SQL and KV host functions are defined in the WIT interface but not yet wired to backing stores.

## Components

| Component | Path | Description |
|---|---|---|
| **Execution Host** | `components/execution-host/` | Rust binary — syncs config from the wp-operator via gRPC, checks the module cache, pulls and AOT-compiles WASM modules on a cache miss, subscribes to a NATS subject, and calls guest exports on each message. |
| **WP Operator** | `components/wp-operator/` | Go operator — watches `Application` CRDs, reconciles database bindings and message subscriptions, and syncs config to execution hosts via a gRPC `ConfigSync` service. |
| **Module Cache** | `components/module-cache/` | Centralized cache for AOT-compiled WASM artifacts, keyed by digest, architecture, and Wasmtime version. |
| **WP Databases** | `components/wp-databases/` | Helm chart — db-operator CRDs that provision the shared PostgreSQL, Redis, and NATS instances. |
| **Hello World** | `examples/hello-world/` | Minimal guest module that implements the `application` world and echoes back request details. |
| **WIT Interface** | `framework/runtime.wit` | The platform's API surface — defines `sql`, `kv`, and `messaging` imports and the `on-message` export. |

## Quick Start

### Prerequisites

- Rust 1.89+ with the `wasm32-wasip2` target:
  ```sh
  rustup target add wasm32-wasip2
  ```

### Local Kubernetes Cluster

```sh
make cluster-up     # Create a k3d cluster with a local registry
tilt up             # Build, deploy, and live-reload on changes
make cluster-down   # Tear down the cluster when done
```

## Documentation

| Document | Purpose |
|----------|---------|
| [docs/architecture.md](docs/architecture.md) | Technology decisions, system design, component responsibilities, and design constraints. |
| [docs/standards.md](docs/standards.md) | Coding conventions, testing strategy, and project-wide rules. |
| [docs/contributions.md](docs/contributions.md) | Development setup, Make targets, project layout, and workflow guides. |

## Contributing

See [docs/contributions.md](docs/contributions.md) for development setup and workflow.

## Open Questions

| Item | Notes |
|---|---|
| Proto versioning strategy | `configsync/v1/` implies a future `v2` is possible. A policy (e.g. bump when a field is removed or semantics change) should be established before the service is live. |
| gRPC service address/port | Not yet in the Helm chart or operator configuration. Needs a `values.yaml` entry and a `ConfigMap`/env-var wiring so the execution host can discover the operator's gRPC endpoint. |
