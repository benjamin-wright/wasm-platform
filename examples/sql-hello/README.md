# sql-hello

A three-function WebAssembly application demonstrating PostgreSQL access via the `sql` WIT interface, using named SQL users with distinct grant sets to verify permission enforcement.

---

## Functions

Each function is a separate compiled module. The Application CR declares two named SQL users (`writer` with ALL on greetings, `reader` with SELECT only) to exercise the named-user code path end-to-end.

### setup

`POST /sql-hello/setup` — bound to the `writer` SQL user.

Seeds the `greetings` table (schema created by the migrations Job before activation):

1. `INSERT INTO greetings (name, active) VALUES ('Alice', true), ('Bob', true), ('Carol', false) ON CONFLICT (name) DO UPDATE SET active = EXCLUDED.active`.

Returns HTTP 200 on success. Idempotent.

### query

`GET /sql-hello` — bound to the `reader` SQL user.

Calls `sql::query("SELECT id, name FROM greetings WHERE active = $1", [boolean(true)])` and returns a JSON array of active rows.

| Export | Example response |
|---|---|
| `on-request` | `[{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]` |

### insert-test

`POST /sql-hello/insert` — bound to the `reader` SQL user.

Attempts `INSERT INTO greetings (name, active) VALUES ('TestUser', true)`. PostgreSQL rejects this with a permission-denied error; the handler maps that to HTTP 403. Any other error returns HTTP 500.

---

## Structure

The three functions live in a sub-workspace under `examples/sql-hello/`. A shared `Cargo.toml` at the workspace root pins the `wit-bindgen` version; each crate inherits it via `wit-bindgen = { workspace = true }`.

```
examples/sql-hello/
  Cargo.toml          ← virtual workspace
  fns/
    setup/            ← sql-hello-setup crate
    query/            ← sql-hello-query crate
    insert-test/      ← sql-hello-insert-test crate
```

---

## Build

```bash
cargo build --manifest-path examples/sql-hello/fns/setup/Cargo.toml \
  --target wasm32-wasip2 --release
cargo build --manifest-path examples/sql-hello/fns/query/Cargo.toml \
  --target wasm32-wasip2 --release
cargo build --manifest-path examples/sql-hello/fns/insert-test/Cargo.toml \
  --target wasm32-wasip2 --release
```

Outputs:
- `target/wasm32-wasip2/release/sql_hello_setup.wasm`
- `target/wasm32-wasip2/release/sql_hello_query.wasm`
- `target/wasm32-wasip2/release/sql_hello_insert_test.wasm`

---

## OCI Packaging

```bash
oras push wasm-platform-registry.localhost:5001/sql-hello-setup:dev \
  target/wasm32-wasip2/release/sql_hello_setup.wasm \
  --artifact-type application/vnd.wasm.content.layer.v1+wasm --plain-http

oras push wasm-platform-registry.localhost:5001/sql-hello-query:dev \
  target/wasm32-wasip2/release/sql_hello_query.wasm \
  --artifact-type application/vnd.wasm.content.layer.v1+wasm --plain-http

oras push wasm-platform-registry.localhost:5001/sql-hello-insert-test:dev \
  target/wasm32-wasip2/release/sql_hello_insert_test.wasm \
  --artifact-type application/vnd.wasm.content.layer.v1+wasm --plain-http
```
