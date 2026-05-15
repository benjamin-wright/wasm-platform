# WASM-Platform

A serverless application platform that runs WebAssembly guest modules on Kubernetes. Guest code interacts with SQL databases, key-value stores, and message queues through a strongly-typed WIT interface — the platform handles provisioning, sandboxing, and scaling.

## Components

| Component | Path | Description |
|---|---|---|
| **Execution Host** | `components/execution-host/` | Rust binary — syncs config from the wp-operator via gRPC, checks the module cache, pulls and AOT-compiles WASM modules on a cache miss, subscribes to NATS subjects, and calls guest exports on each message. |
| **WP Operator** | `components/wp-operator/` | Go operator — watches `Application` CRDs, reconciles database bindings and message subscriptions, and syncs config to execution hosts and the gateway via gRPC. |
| **Gateway** | `components/gateway/` | Rust HTTP server — translates inbound HTTP requests to NATS request-reply based on an operator-pushed route table and returns the response. |
| **Module Cache** | `components/module-cache/` | Rust HTTP service — stores and serves AOT-compiled WASM artifacts keyed by digest, architecture, and Wasmtime version. |
| **CLI** | `components/cli/` | Developer tooling for bootstrapping projects, scaffolding functions and migrations, and building guest modules. |
| **WP Databases** | `components/wp-databases/` | db-operator CRDs that provision the shared PostgreSQL, Redis, and NATS instances (rendered by the umbrella chart). |
| **WIT Interface** | `framework/runtime.wit` | The platform's API surface — defines `sql`, `kv`, `messaging`, `log`, and `metrics` imports and the `on-message` / `on-request` exports across two guest worlds. See [docs/architecture.md](docs/architecture.md) for the design rationale. |
| **Helm Chart** | `helm/wasm-platform/` | Unified Helm chart for all platform components and database CRs. |

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

## Examples

Worked guest module examples live under `examples/`. Each directory contains a `README.md` explaining what the example demonstrates and how to build it.

## Documentation

| Document | Purpose |
|----------|---------|
| [docs/architecture.md](docs/architecture.md) | Technology decisions, system design, component responsibilities, and design constraints. |
| [docs/standards.md](docs/standards.md) | Coding conventions, testing strategy, and project-wide rules. |
| [docs/contributions.md](docs/contributions.md) | Development setup, Make targets, project layout, and workflow guides. |

## Contributing

See [docs/contributions.md](docs/contributions.md) for development setup and workflow.
