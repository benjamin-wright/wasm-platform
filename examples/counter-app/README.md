# counter-app

A single-function WebAssembly application used as a KV isolation fixture. It intentionally uses the same KV store and key names as `demo-app` to verify that each Application's KV namespace is fully isolated.

---

## Functions

### http-handler

Implements the `http-application` world. On each `GET /counter` request it:

1. Emits an `info` log entry.
2. Increments the `requests` counter in the `counter-app` KV store via `kv::incr`.
3. Returns a plain-text response with the counter value.

| Export | Example response |
|---|---|
| `on-request` | `counter-app: requests=3` |

The function uses the same store name (`counters`) and key name (`requests`) as `demo-app`'s http-handler. Because `spec.keyValue` is `counter-app` (not `demo-app`), the two applications operate on separate KV namespaces and their counters are fully independent.

---

## Build

```bash
cargo build \
  --manifest-path examples/counter-app/http-handler/Cargo.toml \
  --target wasm32-wasip2 --release
```

Output: `target/wasm32-wasip2/release/counter_app_http_handler.wasm`

---

## OCI Packaging

```bash
oras push wasm-platform-registry.localhost:5001/counter-app-http:dev \
  target/wasm32-wasip2/release/counter_app_http_handler.wasm \
  --artifact-type application/vnd.wasm.content.layer.v1+wasm \
  --plain-http
```
