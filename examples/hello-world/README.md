# hello-world

A minimal WebAssembly guest module written in Rust that proves the host/guest interface works end-to-end. See the [root README](../../README.md) for project context.

---

## Exports

Implements the `http-application` world from [`framework/runtime.wit`](../../framework/runtime.wit). The handler returns a plain-text response summarising the incoming request.

| Export | Example response |
|---|---|
| `on-request` | `"hello from wasm: method=GET path=/hello"` |

The `sql`, `kv`, and `messaging` imports are satisfied at link time by the execution host but are never called.

---

## Build

```bash
cargo build \
  --manifest-path examples/hello-world/Cargo.toml \
  --target wasm32-wasip2 \
  --release
```

Output: `target/wasm32-wasip2/release/hello_world.wasm`

---

## OCI Packaging

Modules must be pushed as raw OCI artifact layers using `oras`. A gzip-compressed Docker image layer will not work — the execution host's `oci::pull_wasm_bytes` reads the raw blob bytes directly.

Install `oras` once (`brew install oras`), then:

```bash
oras push wasm-platform-registry.localhost:5001/hello-world:dev \
  target/wasm32-wasip2/release/hello_world.wasm \
  --artifact-type application/vnd.wasm.content.layer.v1+wasm \
  --plain-http
```

The `--plain-http` flag is required because the local registry does not use TLS. The `spec.module` field in the Application CR must use a plain registry reference (no `oci://` scheme prefix): `wasm-platform-registry.localhost:5001/hello-world:dev`.
