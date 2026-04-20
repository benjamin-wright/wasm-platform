#!/usr/bin/env bash
# setup-sql-hello.sh — creates the examples/sql-hello directory structure.
# Run once from the repo root after cloning to populate the sql-hello example.
# This script is idempotent; it is safe to run multiple times.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
EXAMPLE_DIR="${REPO_ROOT}/examples/sql-hello"

echo "Creating examples/sql-hello ..."

mkdir -p \
  "${EXAMPLE_DIR}/http-handler/src" \
  "${EXAMPLE_DIR}/k8s"

# ── http-handler/Cargo.toml ──────────────────────────────────────────────────

cat > "${EXAMPLE_DIR}/http-handler/Cargo.toml" <<'EOF'
[package]
name = "sql-hello-http-handler"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib"]

[dependencies]
wit-bindgen = "0.36"
EOF

# ── http-handler/src/lib.rs ──────────────────────────────────────────────────

cat > "${EXAMPLE_DIR}/http-handler/src/lib.rs" <<'EOF'
wit_bindgen::generate!({
    world: "http-application",
    path: "../../../framework/runtime.wit",
});

use framework::runtime::{log, sql};

struct SqlHelloHandler;

impl Guest for SqlHelloHandler {
    fn on_request(_request: HttpRequest) -> Result<HttpResponse, String> {
        log::emit(log::Level::Info, "handling sql-hello request");

        let rows = sql::query(
            "SELECT id, name FROM greetings WHERE active = $1",
            &[sql::Param::Boolean(true)],
        )?;

        let mut items = Vec::with_capacity(rows.len());
        for row in &rows {
            let id = match row.values.get(0) {
                Some(sql::Param::Integer(n)) => *n,
                Some(sql::Param::Null) => return Err("null id".to_string()),
                _ => return Err("unexpected type for id column".to_string()),
            };
            let name = match row.values.get(1) {
                Some(sql::Param::Text(s)) => s.clone(),
                Some(sql::Param::Null) => return Err("null name".to_string()),
                _ => return Err("unexpected type for name column".to_string()),
            };
            items.push(format!("{{\"id\":{id},\"name\":{name:?}}}"));
        }

        let body = format!("[{}]", items.join(","));

        Ok(HttpResponse {
            status: 200,
            headers: vec![("content-type".to_string(), "application/json".to_string())],
            body: Some(body.into_bytes()),
        })
    }
}

export!(SqlHelloHandler);
EOF

# ── k8s/application.yaml ────────────────────────────────────────────────────

cat > "${EXAMPLE_DIR}/k8s/application.yaml" <<'EOF'
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: sql-hello
  namespace: default
spec:
  sql: {}   # implicit 'app' user, ALL on all tables
  functions:
    - name: handler
      module: sql-hello-http
      # sqlUser omitted — implicit 'app' binding under spec.sql: {}
      trigger:
        http:
          path: /sql-hello
          methods:
            - GET
EOF

# ── k8s/seed-job.yaml ────────────────────────────────────────────────────────
# Seeds the greetings table used by the sql-hello module.
# The Job uses the PostgresCredential Secret created by the wp-operator when
# the Application CR above is applied. It retries until the Secret exists
# (the operator sets Application Ready=True only after the Secret is available).
# Run this Job after the sql-hello Application is deployed.

cat > "${EXAMPLE_DIR}/k8s/seed-job.yaml" <<'EOF'
apiVersion: batch/v1
kind: Job
metadata:
  # Named deterministically so re-deploys are idempotent (Job already exists → no-op).
  name: sql-hello-seed
  namespace: wasm-platform
spec:
  backoffLimit: 10
  template:
    spec:
      restartPolicy: OnFailure
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      containers:
        - name: seed
          image: postgres:15
          securityContext:
            allowPrivilegeEscalation: false
            capabilities:
              drop: ["ALL"]
          command:
            - /bin/sh
            - -c
            - |
              set -e
              until pg_isready -h "$PGHOST" -p "$PGPORT" -U "$PGUSER"; do
                echo "waiting for postgres..."
                sleep 2
              done
              psql -c "
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
              "
          envFrom:
            - secretRef:
                # Secret created by the db-operator for the 'app' SQL user.
                # Provides: PGUSER, PGPASSWORD, PGHOST, PGPORT.
                name: wasm-default-sql-hello-app-pg-creds
          env:
            - name: PGDATABASE
              value: wasm_default__sql_hello
EOF

# ── Tiltfile ─────────────────────────────────────────────────────────────────

cat > "${EXAMPLE_DIR}/Tiltfile" <<'EOF'
def sql_hello(namespace, resource_deps=[]):
    k8s_kind('Application',
             image_json_path = '{.spec.functions[*].module}',
             pod_readiness = 'ignore')

    custom_build(
        'sql-hello-http',
        command = (
            'cargo build --manifest-path examples/sql-hello/http-handler/Cargo.toml' +
            ' --target wasm32-wasip2 --release --locked &&' +
            ' oras push $EXPECTED_REF' +
            ' target/wasm32-wasip2/release/sql_hello_http_handler.wasm' +
            ' --artifact-type application/vnd.wasm.content.layer.v1+wasm' +
            ' --plain-http'
        ),
        deps = [
            'examples/sql-hello/http-handler/src',
            'examples/sql-hello/http-handler/Cargo.toml',
            'Cargo.toml',
            'Cargo.lock',
            'framework/runtime.wit',
        ],
        skips_local_docker = True,
    )

    k8s_yaml('examples/sql-hello/k8s/application.yaml')
    k8s_yaml('examples/sql-hello/k8s/seed-job.yaml')

    k8s_resource(
        'sql-hello',
        resource_deps = ['module-cache', 'wp-operator'] + resource_deps,
        labels = ['example'],
    )

    k8s_resource(
        'sql-hello-seed',
        resource_deps = ['sql-hello'],
        labels = ['example'],
    )
EOF

# ── README.md ─────────────────────────────────────────────────────────────────

cat > "${EXAMPLE_DIR}/README.md" <<'EOF'
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
EOF

# ── Patch Cargo.toml ──────────────────────────────────────────────────────────

CARGO_TOML="${REPO_ROOT}/Cargo.toml"
if ! grep -q 'examples/sql-hello/http-handler' "${CARGO_TOML}"; then
    sed -i 's|    "examples/counter-app/http-handler",|    "examples/counter-app/http-handler",\n    "examples/sql-hello/http-handler",|' "${CARGO_TOML}"
    echo "Patched Cargo.toml: added examples/sql-hello/http-handler workspace member."
else
    echo "Cargo.toml already contains sql-hello; skipping."
fi

# ── Patch root Tiltfile ───────────────────────────────────────────────────────

ROOT_TILTFILE="${REPO_ROOT}/Tiltfile"
if ! grep -q 'sql_hello' "${ROOT_TILTFILE}"; then
    sed -i "s|load('./examples/counter-app/Tiltfile', 'counter_app')|load('./examples/counter-app/Tiltfile', 'counter_app')\nload('./examples/sql-hello/Tiltfile', 'sql_hello')|" "${ROOT_TILTFILE}"
    sed -i "s|counter_app('examples', resource_deps=\['wp-operator', 'execution-host', 'gateway'\])|counter_app('examples', resource_deps=['wp-operator', 'execution-host', 'gateway'])\nsql_hello('examples', resource_deps=['wp-operator', 'execution-host', 'gateway'])|" "${ROOT_TILTFILE}"
    echo "Patched root Tiltfile: added sql_hello."
else
    echo "Root Tiltfile already contains sql_hello; skipping."
fi

echo ""
echo "Done. examples/sql-hello created and Cargo.toml + Tiltfile patched."
echo ""
echo "Next: git add . && git commit -m 'feat: add sql-hello example'"
