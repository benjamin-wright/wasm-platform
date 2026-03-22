# hello-world

A minimal WebAssembly guest module written in Rust that proves the host/guest interface works end-to-end. See the [root README](../../README.md) for project context.

---

## Exports

Implements the `application` world from [`framework/runtime.wit`](../../framework/runtime.wit). The handler echoes back a human-readable summary of its payload.

| Export | Example response |
|---|---|
| `on-message` | `"on-message called: payload='hello'"` |

Returning `Some(bytes)` sends the bytes back to the caller as a response (e.g. an HTTP reply or a NATS reply message). Returning `None` is a fire-and-forget acknowledgement with no response body.

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
