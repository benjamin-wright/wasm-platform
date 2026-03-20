# Contributing

## Prerequisites

- Rust 1.89+ (with `wasm32-wasip2` target installed)
- [k3d](https://k3d.io)
- [Tilt](https://tilt.dev)
- Helm
- curl (for manual smoke tests)

Install the WASM target if you haven't already:

```sh
rustup target add wasm32-wasip2
```

## Local Cluster

```sh
make cluster-up      # Create k3d cluster with registry
make cluster-down    # Tear down the cluster
```

After `cluster-up`, export the kubeconfig it prints (or use direnv):

```sh
export KUBECONFIG=~/.scratch/wasm-platform.yaml
```

## Tilt

```sh
tilt up
```

Builds container images, deploys the Helm chart, and live-reloads on source changes. Requires a running cluster from `make cluster-up`.

## Running Locally (no cluster)

For quick iteration without Kubernetes:

```sh
make run             # Builds the hello-world guest, then runs the execution host
```

In a separate terminal:

```sh
make test            # Sends a sample request to the running execution host
```

## Make Targets

| Target | Description |
|--------|-------------|
| `make hello` | Build the hello-world guest module to `target/wasm32-wasip2/release/hello_world.wasm` |
| `make run` | Build the guest, then run the execution host locally |
| `make test` | Send a sample HTTP request to a running execution host |
| `make cluster-up` | Create the local k3d cluster and registry |
| `make cluster-down` | Tear down the local k3d cluster and registry |

## Project Layout

```
├── Cargo.toml                  # Workspace root
├── Makefile                    # Build and cluster targets
├── framework/
│   └── runtime.wit             # WIT interface — the platform's API surface
├── components/
│   └── execution-host/         # Rust binary — loads and invokes WASM modules
├── examples/
│   └── hello-world/            # Minimal guest module for testing the interface
├── helm/                       # Helm charts (planned)
├── docs/
│   ├── standards.md            # Coding conventions and technical decisions
│   └── contributions.md        # This file
└── Tiltfile                    # Live development config (planned)
```

## Workflow

### Adding a New Guest Example

1. Create a new crate under `examples/` with `crate-type = ["cdylib"]`.
2. Add `wit-bindgen` as a dependency and generate bindings for the `application` world from `framework/runtime.wit`.
3. Implement the `Guest` trait (`on-request`, `on-schedule`, `on-message`).
4. Add the crate to the workspace `members` in the root `Cargo.toml`.
5. Build with `cargo build --manifest-path examples/<name>/Cargo.toml --target wasm32-wasip2 --release`.
6. Add a `README.md` documenting what the example demonstrates.

### Adding a New Component

1. Create a new crate under `components/`.
2. Add it to the workspace `members` in the root `Cargo.toml`.
3. Add a `README.md` describing its observable behaviour and interfaces.
4. Add a Makefile target if it has a standalone build or run step.

### Modifying the WIT Interface

The `framework/runtime.wit` file is the platform's API contract. Changing it is a breaking change for all guest modules and the execution host. Before modifying:

1. Check whether the change can be made backwards-compatible (adding new functions is safe; changing signatures is not).
2. Update both the host (`components/execution-host`) and all guest examples to match.
3. Rebuild and test everything: `make run` then `make test`.

## Standards

See [docs/standards.md](standards.md) for coding conventions, testing strategy, and architecture guidelines.
