# WASM-Platform

A serverless application platform that runs WebAssembly guest modules on Kubernetes. Guest code interacts with SQL databases, key-value stores, and message queues through a strongly-typed WIT interface — the platform handles provisioning, sandboxing, and scaling.

## Current Status — Phase 0 (Proof of Concept)

The project is in its earliest phase: a single Rust binary that loads a `.wasm` guest module and invokes it over HTTP. SQL and KV host functions are defined in the WIT interface but not yet wired to backing stores.

## Components

| Component | Path | Description |
|---|---|---|
| **Execution Host** | `components/execution-host/` | Rust binary — receives config from the wp-operator, checks the module cache, pulls and AOT-compiles WASM modules on a cache miss, exposes an HTTP endpoint, and calls guest exports. |
| **WP Operator** | `components/wp-operator/` | Go operator — watches `Application` CRDs, reconciles database bindings and message subscriptions, and pushes config updates directly to execution hosts. |
| **Module Cache** | `components/module-cache/` | Centralized cache for AOT-compiled WASM artifacts, keyed by digest, architecture, and Wasmtime version. |
| **Hello World** | `examples/hello-world/` | Minimal guest module that implements the `application` world and echoes back request details. |
| **WIT Interface** | `framework/runtime.wit` | The platform's API surface — defines `sql`, `kv` imports and `on-request`, `on-schedule`, `on-message` exports. |

## Quick Start

### Prerequisites

- Rust 1.89+ with the `wasm32-wasip2` target:
  ```sh
  rustup target add wasm32-wasip2
  ```

### Run Locally

```sh
make run        # Builds the hello-world guest, then starts the execution host on :3000
```

In a separate terminal:

```sh
make test       # Sends a sample POST to /execute and prints the response
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
