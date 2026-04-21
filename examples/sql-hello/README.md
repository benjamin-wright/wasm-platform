# sql-hello

A single-function WebAssembly application demonstrating PostgreSQL access via the `sql` WIT interface. The function queries an existing `greetings` table and returns active rows as JSON.

---

## Function

### http-handler

Implements the `http-application` world. On each `GET /sql-hello` request it:

1. Calls `sql::query("SELECT id, name FROM greetings WHERE active = $1", [boolean(true)])`.
2. Returns a JSON array of `{"id": N, "name": "..."}` objects for each active row.

| Export | Example response |
|---|---|
| `on-request` | `[{"id":1,"name":"Alice"},{"id":2,"name":"Bob"}]` |

The Application CR uses `spec.sql: {}` (implicit `app` user, ALL on all tables). No `sqlUser` field is required on the function.

---

## Database schema

Seeded by `k8s/seed-job.yaml`, deployed to the `wasm-platform` namespace using the PG credentials from the `wasm-default-sql-hello-app-pg-creds` Secret:

```sql
CREATE TABLE IF NOT EXISTS greetings (
  id     serial PRIMARY KEY,
  name   text   NOT NULL UNIQUE,
  active bool   NOT NULL DEFAULT true
);
INSERT INTO greetings (name, active) VALUES
  ('Alice', true),
  ('Bob',   true),
  ('Carol', false)
ON CONFLICT (name) DO UPDATE SET active = EXCLUDED.active;
```

---

## Build

```bash
cargo build \
  --manifest-path examples/sql-hello/http-handler/Cargo.toml \
  --target wasm32-wasip2 --release
```

Output: `target/wasm32-wasip2/release/sql_hello_http_handler.wasm`

---

## OCI Packaging

```bash
oras push wasm-platform-registry.localhost:5001/sql-hello-http:dev \
  target/wasm32-wasip2/release/sql_hello_http_handler.wasm \
  --artifact-type application/vnd.wasm.content.layer.v1+wasm \
  --plain-http
```
