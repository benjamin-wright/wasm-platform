# demo-app

A two-function WebAssembly application demonstrating multi-function deployment on the platform. Each function is a separate `.wasm` module compiled to `wasm32-wasip2` and referenced by a single Application CR.

---

## Functions

### http-handler

Implements the `http-application` world. On each `GET /hello` request it:

1. Emits an `info` log entry.
2. Publishes a `tick` event to `demo-app.events` via `messaging::send`.
3. Increments the `requests` counter in the `demo-app` KV store via `kv::incr`.
4. Reads the `messages` counter written by `message-handler`.
5. Returns a plain-text response with both counter values.

| Export | Example response |
|---|---|
| `on-request` | `hello from wasm: method=GET path=/hello requests=3 messages=2` |

### message-handler

Implements the `message-application` world. On each message received on topic `demo-app.events` it:

1. Emits an `info` log entry.
2. Increments the `messages` counter in the `demo-app` KV store via `kv::incr`.

The two functions share the same KV namespace (`spec.keyValue: demo-app`), so `http-handler` can read the counter that `message-handler` writes.

---

## Build

```bash
# http-handler
cargo build \
  --manifest-path examples/demo-app/http-handler/Cargo.toml \
  --target wasm32-wasip2 --release

# message-handler
cargo build \
  --manifest-path examples/demo-app/message-handler/Cargo.toml \
  --target wasm32-wasip2 --release
```

Outputs:
- `target/wasm32-wasip2/release/http_handler.wasm`
- `target/wasm32-wasip2/release/message_handler.wasm`

---

## OCI Packaging

Push each module as a raw OCI artifact layer using `oras`:

```bash
oras push wasm-platform-registry.localhost:5001/demo-app-http:dev \
  target/wasm32-wasip2/release/http_handler.wasm \
  --artifact-type application/vnd.wasm.content.layer.v1+wasm \
  --plain-http

oras push wasm-platform-registry.localhost:5001/demo-app-messages:dev \
  target/wasm32-wasip2/release/message_handler.wasm \
  --artifact-type application/vnd.wasm.content.layer.v1+wasm \
  --plain-http
```
