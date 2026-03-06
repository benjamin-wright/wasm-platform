# hello-world

A minimal WebAssembly guest module written in Rust that proves the host/guest interface works end-to-end. See the [root README](../../README.md) for project context.

---

## Exports

Implements the `application` world from [`framework/runtime.wit`](../../framework/runtime.wit). Each handler echoes back a human-readable summary of its arguments.

| Export | Example response |
|---|---|
| `on-request` | `"on-request called: method=GET path=/hello body=5 bytes"` |
| `on-schedule` | `"on-schedule called: name=daily-cleanup"` |
| `on-message` | `"on-message called: queue=events payload=12 bytes"` |

The `sql` and `kv` imports are satisfied at link time by the execution host but are never called.

---

## Build

```bash
cargo build \
  --manifest-path examples/hello-world/Cargo.toml \
  --target wasm32-wasip2 \
  --release
```

Output: `target/wasm32-wasip2/release/hello_world.wasm`
