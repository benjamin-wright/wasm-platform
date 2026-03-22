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

Builds container images, deploys Helm charts, runs unit tests on source change, and live-reloads on source changes. Requires a running cluster from `make cluster-up`.

Resources are grouped by component label in the Tilt UI. Integration tests appear under the same label and must be triggered manually — click the resource and press the trigger button after the service is ready.

Each component's build, deploy, and test logic lives in `components/<name>/Tiltfile`. The root `Tiltfile` loads and calls each component function.

## Make Targets

| Target | Description |
|--------|-------------|
| `make cluster-up` | Create the local k3d cluster and registry |
| `make cluster-down` | Tear down the local k3d cluster and registry |
| `make hello` | Build the hello-world guest module to `target/wasm32-wasip2/release/hello_world.wasm` |
| `make run` | Build the guest, then run the execution host locally (no cluster) |
| `make docker-build` | Build the execution-host container image and tag it for the local registry |

## Project Layout

```
├── Cargo.toml                  # Workspace root
├── Makefile                    # Build and cluster targets
├── framework/
│   └── runtime.wit             # WIT interface — the platform's API surface
├── components/
│   ├── execution-host/         # Rust binary — loads and invokes WASM modules
│   │   ├── Tiltfile            # Defines execution_host() for the root Tiltfile
│   │   └── helm/               # Helm chart for the execution host
│   └── crd-operator/           # Go operator — reconciles Application CRDs
├── examples/
│   └── hello-world/            # Minimal guest module for testing the interface
├── Tiltfile                    # Root live-development entrypoint — loads component Tiltfiles
├── .dockerignore               # Excludes target/ and .git/ from Docker build context
├── docs/
│   ├── standards.md            # Coding conventions and technical decisions
│   └── contributions.md        # This file
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
4. Add a `Dockerfile` at `components/<name>/Dockerfile` following the container image standards.
5. Add a Helm chart at `components/<name>/helm/` following the Helm chart standards.
6. Add a `Tiltfile` at `components/<name>/Tiltfile` following the Tilt standards. Define a single public function named `<dir_snake_case>()` that encapsulates all resources for the component.
7. Load and call the new function in the root `Tiltfile`.

### Modifying the WIT Interface

The `framework/runtime.wit` file is the platform's API contract. Changing it is a breaking change for all guest modules and the execution host. Before modifying:

1. Check whether the change can be made backwards-compatible (adding new functions is safe; changing signatures is not).
2. Update both the host (`components/execution-host`) and all guest examples to match.
3. Rebuild and test everything: `tilt up`, then trigger the integration test for each component manually in the Tilt UI.

## Standards

See [docs/standards.md](standards.md) for coding conventions, testing strategy, and architecture guidelines.
