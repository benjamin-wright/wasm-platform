# TODO

Active implementation plan for the wasm-platform project.

---

## Execution Host: Align Implementation with README Spec

Each phase is independently launchable in its own agent session. The permanent regression guard throughout is the hello-world e2e fixture (HTTP trigger, KV counter) — it must pass at every phase boundary. After every phase, the `e2e-tests` resource must pass (trigger via the Tilt MCP server).

---

### Phase 9.2: SQL Host Function (No Migrations)

Implement the `sql` WIT interface host functions backed by per-app, per-user PostgreSQL
connection pools. The database name is never user-visible — derived deterministically from
`(namespace, app_name)`. The operator manages `PostgresCredential` lifecycle via the
db-operator, which generates randomised passwords; the operator assembles the full
connection URL before pushing config. No migrations machinery in this phase (Phase 9.3
adds the implicit `migrations` user and Job lifecycle).

#### Design

**CRD shape — `spec.sql`**

`spec.sql` replaces the existing `spec.sql string` field with an optional struct
(`*SQLSpec`). Since OpenAPI v3 does not support sum types, `spec.sql: {}` (empty struct)
is the shorthand for "SQL enabled with defaults". This is a **breaking CRD change**; the
hello-world Application CR and any e2e fixtures require updating.

```yaml
# Minimal: one implicit 'app' user, ALL on all tables.
# Functions with no sqlUser field are automatically bound to 'app'.
sql: {}

# Custom: named users with per-table grants.
# Functions must set sqlUser to get SQL access; omitting sqlUser is a valid opt-out.
sql:
  users:
    - name: reader
      permissions:
        - tables: [orders, line_items]
          grant: [SELECT]
    - name: writer
      permissions:
        - tables: [orders, line_items]
          grant: [SELECT, INSERT, UPDATE, DELETE]
```

Operator semantics:
- `spec.sql` absent → no SQL access provisioned.
- `spec.sql: {}` (`users` omitted or empty) → operator synthesises a single user named
  `app` with `ALL` on all tables in the app's database. Functions with no `sqlUser` field
  are implicitly bound to `app`.
- `spec.sql.users` non-empty → operator creates exactly the listed users. A function with
  no `sqlUser` gets no SQL access; SQL calls return `Err` at runtime.

CEL validation rules:
- `migrations` is reserved in `spec.sql.users[*].name` (Phase 9.3 adds it implicitly).
- When `spec.sql.users` is non-empty, each function's `sqlUser` value must match a
  defined user name or be absent.

**Per-function `sqlUser` field**

```yaml
functions:
  - name: api
    module: oci://…
    sqlUser: writer
    trigger:
      http: { path: /orders, methods: [POST] }
  - name: reporter
    module: oci://…
    sqlUser: reader
    trigger:
      topic: orders.report
  - name: notifier
    module: oci://…
    # no sqlUser — SQL calls fail at runtime; module has no DB access
    trigger:
      topic: orders.notify
```

**PG identifier derivation**

Both the operator and execution host must use the same algorithm — divergence silently
targets the wrong database. Algorithm:

- *Database name:* `wasm_<namespace>_<app_name>` with `-` → `_`.
- *PG username:* `wasm_<namespace>_<app_name>_<user_name>` with `-` → `_`.
- *Truncation (both):* if the result exceeds 63 characters, take the first 47 characters,
  append `_`, then the first 15 hex characters of the lowercase SHA-256 of the full
  pre-truncation string.

The operator surfaces the derived database name and per-user PG usernames in Application
status for observability.

**WIT interface changes**

Two changes made in a single WIT edit (both are breaking; batching avoids a second break):

1. Remove `db: string` from `query` and `execute`.
2. Add `boolean(bool)` to the `param` variant.

```wit
interface sql {
    variant param {
        null,
        boolean(bool),
        integer(s64),
        real(f64),
        text(string),
        blob(list<u8>),
    }

    record row { columns: list<string>, values: list<param> }
    query:   func(sql: string, params: list<param>) -> result<list<row>, string>;
    execute: func(sql: string, params: list<param>) -> result<u64, string>;
}
```

All guest modules using `sql` require recompilation; none exist in production today. A
follow-up phase should add pagination and row-by-row iteration (a `next-row` streaming
interface) to avoid materialising large result sets in linear memory.

**Config sync — proto changes**

Remove `SqlConfig` (with its `database_name` + `connection_url`). Replace with:

```proto
message SqlUserConfig {
  string username       = 1;  // derived PG username (≤63 chars) — used as pool key
  string connection_url = 2;  // postgres://user:pass@host:port/dbname, built by operator
}

// In ApplicationConfig: replace field 5 (old SqlConfig) with:
repeated SqlUserConfig sql_users = 5;

// In FunctionConfig: add:
optional string sql_username = 6;  // references an entry in ApplicationConfig.sql_users
```

The operator assembles `connection_url` from the db-operator Secret fields
(`PGUSER`, `PGPASSWORD`, `PGHOST`, `PGPORT`) plus the derived database name. Passwords
are encapsulated in the URL and never appear as separate config-sync fields. The execution
host no longer needs `PG_HOST`/`PG_PORT` env vars; remove them from the env var table.

**Credential lifecycle (multi-user)**

For each user in `spec.sql.users` (or the synthetic `app` user for `spec.sql: {}`), the
operator creates one `PostgresCredential` CR:
- `metadata.name`: `wasm-<namespace>-<app_name>-<user_name>-pg` (Kubernetes name limit
  253 chars; apply the same hash-truncation scheme at 238 chars to leave room for suffix).
- `spec.databaseRef`: `Config.PostgresDatabaseName` helm value.
- `spec.username`: derived PG username.
- `spec.secretName`: `wasm-<namespace>-<app_name>-<user_name>-pg-creds`.
- `spec.permissions`: `DatabasePermissionEntry` with `databases: [<derived_db_name>]` and
  the declared grants (or `[ALL]` for the implicit `app` user).

The db-operator creates the logical database idempotently on first `PostgresCredential`
reconciliation; subsequent credentials targeting the same database name are safe.

The operator waits for **all** credentials to reach `Ready` phase before pushing
`ApplicationConfig.sql_users`. While any credential is `Pending`, return
`RequeueAfter: 5s`. On Application deletion, delete all associated `PostgresCredential`
CRs.

**Guard: `PostgresDatabaseName` config**

If `spec.sql` is set and `Config.PostgresDatabaseName` is empty, the reconciler sets
`Ready: False` with reason `DatabaseConfigMissing` rather than producing a malformed CR.
At reconcile time the operator performs a `GET` on the named `PostgresDatabase` CR; if
not found, sets `Ready: False` with reason `DatabaseNotFound`. Requires adding
`get;list;watch` on `postgresdatabases` to the RBAC rules.

**Execution host — connection pools**

- Crates: `sqlx` (features: `postgres`, `runtime-tokio-rustls`), `dashmap`.
- Pool map: `Arc<DashMap<(namespace, app_name, username), PgPool>>` in `RuntimeState`.
- Pools are created **eagerly on config arrival** (full snapshot and incremental) to
  minimise invocation cold-start.
- Pool max size: `PG_POOL_MAX_CONNECTIONS` env var (default `5`). Note: the current
  uniform Deployment model is a deliberate simplification. Intelligent scaling (KEDA
  event-driven scaling, asymmetric function-to-host placement based on capacity measures)
  is a future concern to revisit once load profiles are known.
- On config delete: call `PgPool::close()` on all pools for the evicted app; remove from
  map.
- Per invocation: look up pool by `(namespace, app_name, sql_username)`; pass
  `Option<PgPool>` into `HostState`. `None` if the function has no `sql_username`.

**Param binding**

| WIT `param` | sqlx binding |
|---|---|
| `null` | `Option::<String>::None` |
| `boolean(bool)` | `bool` |
| `integer(s64)` | `i64` |
| `real(f64)` | `f64` |
| `text(string)` | `String` |
| `blob(list<u8>)` | `Vec<u8>` |

**Row deserialisation**

Columns read by index. PG → `param` mapping: `bool` → `boolean`, `int2`/`int4`/`int8` →
`integer`, `float4`/`float8` → `real`, `text`/`varchar`/`bpchar`/`name` → `text`,
`bytea` → `blob`, `NULL` → `null`. Any column with an unmapped PG type returns `Err` for
the whole row (no panic).

**`sql-hello` example module**

An `http-application` world Rust module:

```rust
// on_request: SELECT id, name FROM greetings WHERE active = $1
// params: [boolean(true)]  →  JSON array of {id, name} objects
```

Application CR:
```yaml
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: sql-hello
  namespace: default
spec:
  sql: {}    # implicit 'app' user, ALL on all tables
  functions:
    - name: handler
      module: oci://…
      # sqlUser omitted — implicit 'app' binding under spec.sql: {}
      trigger:
        http:
          path: /sql-hello
          methods: [GET]
```

E2e fixture: a Kubernetes Job seeds a `greetings(id serial, name text, active bool)` table
before the Application is deployed. The test GETs `/sql-hello` and asserts the response
contains the expected rows. The e2e test is the primary coverage vehicle for SQL param
binding, row deserialisation, and the pool lifecycle.

**`sql-hello` redesign (pending — current implementation incomplete)**

The seed-Job approach was found to be fragile due to a secret naming mismatch and was
abandoned mid-phase. The agreed replacement uses three HTTP handler functions in the same
Application, removing any external seeding dependency.

*Motivation:* A Job is a second external system that must agree on naming conventions
with the operator. An HTTP handler is self-contained inside the Application boundary and
uses only the pool the execution host provides — no external coordination needed. Using
named users (`writer` and `reader`) also tests the named-user path properly: multiple
`PostgresCredential` CRs, per-function pool lookup by `sql_username`, and grant
enforcement are all the distinguishing behaviours of Phase 9.2. The current `spec.sql: {}`
implicit `app` user path tests almost nothing distinctive.

*Constraint:* `migrations` is reserved in `spec.sql.users[*].name` by the Phase 9.2 CEL
rule. The DDL user must use a different name (e.g. `writer`).

*Application CR shape:*
```yaml
spec:
  sql:
    users:
      - name: writer
        permissions:
          - tables: [greetings]
            grant: [ALL]
      - name: reader
        permissions:
          - tables: [greetings]
            grant: [SELECT]
  functions:
    - name: setup
      module: sql-hello-http
      sqlUser: writer
      trigger:
        http: { path: /sql-hello/setup, methods: [POST] }
    - name: query
      module: sql-hello-http
      sqlUser: reader
      trigger:
        http: { path: /sql-hello, methods: [GET] }
    - name: insert-test
      module: sql-hello-http
      sqlUser: reader
      trigger:
        http: { path: /sql-hello/insert, methods: [POST] }
```

*Function behaviour:*
- `setup` (`writer`): `CREATE TABLE IF NOT EXISTS greetings …` + `INSERT … ON CONFLICT DO
  UPDATE`. Idempotent. Returns 200 on success.
- `query` (`reader`): `SELECT id, name FROM greetings WHERE active = $1` with
  `[boolean(true)]`. Returns JSON array of active rows.
- `insert-test` (`reader`): attempts `INSERT INTO greetings …`. PostgreSQL will return a
  permission-denied error; the handler maps that to HTTP 403. Any other error returns 500.

*E2e test flow:*
1. `POST /sql-hello/setup` — wait for 200.
2. `GET /sql-hello` — assert Alice and Bob present, Carol absent.
3. `POST /sql-hello/insert` — assert 403 (permission denied enforced by PostgreSQL).
`TestMain` waits only on the `sql-hello` Application Ready condition; no seed Job.

*`spec.sql: {}` (implicit `app` user) coverage:* This path is no longer exercised by the
e2e test. Confirm it is covered by operator unit tests before closing the phase.

*Delete existing files* before implementing the redesign:
- `examples/sql-hello/k8s/seed-job.yaml` — replaced by the `setup` handler.
- The `sql-hello-seed` Tiltfile resource block.

#### Tasks

- [x] **WIT**: remove `db: string` from `sql.query` / `sql.execute`; add `boolean(bool)`
  to the `param` variant in `framework/runtime.wit`. Update all `bindgen!` call sites in
  execution-host.
- [x] **Cargo**: add `sqlx` (features: `postgres`, `runtime-tokio-rustls`) and `dashmap`
  to `components/execution-host/Cargo.toml`.
- [x] **CRD**: replace `spec.sql string` with `spec.sql *SQLSpec` (optional struct with
  optional `users` list); add `sqlUser *string` to `FunctionSpec`. Add CEL rules:
  (a) `migrations` reserved in `spec.sql.users[*].name`; (b) when `spec.sql.users` is
  non-empty, each function's `sqlUser` must be a defined user name or absent. Run
  `make generate` in `components/wp-operator/`.
- [x] **Proto**: remove `SqlConfig`; add `SqlUserConfig` (username + connection_url) and
  `repeated sql_users` (field 5) to `ApplicationConfig`; add `optional sql_username`
  (field 6) to `FunctionConfig`. Regenerate stubs.
- [x] **Operator — RBAC**: add `get;list;watch` on `postgresdatabases` to the RBAC rules
  and Helm chart.
- [x] **Operator — derivation utility**: implement and unit-test the PG identifier
  derivation algorithm (database name and username, with hash-truncation) as a shared
  helper in the controller package.
- [x] **Operator — `reconcileSQLBinding`**: use the derivation utility; create one
  `PostgresCredential` CR per user (including the synthetic `app` user for `spec.sql: {}`);
  inject `sql_username` into `FunctionConfig` for implicitly-bound functions; wait for all
  credentials to reach `Ready`; assemble `connection_url` from the db-operator Secret;
  populate `ApplicationConfig.sql_users`; guard against empty `PostgresDatabaseName` and
  missing `PostgresDatabase` CR.
- [x] **Operator — delete path**: delete all associated `PostgresCredential` CRs on
  Application deletion.
- [x] **Operator — status**: surface derived database name and per-user PG usernames in
  Application status.
- [x] **Execution host — `host_sql.rs`**: implement the `sql` WIT trait for `HostState`.
  `query` binds params and deserialises rows per tables above; returns `Err` for unmapped
  column types. `execute` returns row count. Both return `Err` if `HostState.sql_pool`
  is `None`.
- [x] **Execution host — pool map**: add `SqlPoolMap` and `PG_POOL_MAX_CONNECTIONS` env
  var to `RuntimeState`; create pools eagerly on config arrival; evict and close on
  delete; resolve and pass `Option<PgPool>` into `HostState` per invocation.
- [x] **Execution host — linker**: register `sql::add_to_linker` once via
  `message_bindings` in `RuntimeState::new` (the wasmtime Linker keys on WIT interface
  name, so a single registration covers both `message-application` and `http-application`
  worlds — same pattern as `kv`/`log`/`messaging`/`metrics`).
- [x] **`sql-hello` example**: implement module, Application CR, and seeding Job.
  (Code in `setup-sql-hello.sh`; run `bash setup-sql-hello.sh` from repo root to create `examples/sql-hello/`.)
- [x] **e2e test**: seeding Job runs first; assert GET `/sql-hello` returns expected rows;
  hello-world test is unaffected.
- [x] Update `components/execution-host/README.md`: add SQL to the Data Isolation table;
  document `PG_POOL_MAX_CONNECTIONS`; remove `PG_HOST`/`PG_PORT` from the env var table.
- [x] Update `components/wp-operator/README.md`: document new `spec.sql` struct shape,
  `sqlUser` function field, PG identifier derivation algorithm, reserved names, credential
  lifecycle, and new status fields.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

#### Verification

New e2e test (`sql-hello`) passes. `e2e-tests` resource passes. hello-world e2e test is
unaffected.

---

### Phase 9.3a: db-operator Prerequisites (external)

Tracked in the db-operator workspace, not here. Required before 9.3c can begin:

1. `PostgresCredential.spec.databaseOwner` field — lets the per-app `migrations`
   credential become the database owner, enabling DDL and propagating default
   privileges to peer credentials.
2. `pg_advisory_lock` around the migrations runner — serialises concurrent Job pods.

See `db-operator/docs/todo.md` (`Migrations Owner Role + Concurrency Safety`). Pin the
resulting db-operator chart version in 9.3c.

---

### Phase 9.3b: Migrations Packaging Documentation

Document how application authors package their `.sql` files into a runnable migrations
image. No platform code changes — pure docs + a worked example.

#### Design

**Developer workflow (target experience)**

1. Author writes paired SQL files in a project subdirectory, e.g. `migrations/`:
   ```
   migrations/
     001-create-greetings-apply.sql
     001-create-greetings-rollback.sql
     002-add-active-column-apply.sql
     002-add-active-column-rollback.sql
   ```
2. Author writes **one** Dockerfile per repo (typically a monorepo with several apps),
   parameterised by build-arg, that produces an image per app:
   ```dockerfile
   ARG DB_MIGRATIONS_VERSION=0.2.0
   FROM ghcr.io/benjamin-wright/db-operator/db-migrations:${DB_MIGRATIONS_VERSION}
   ARG APP
   COPY ${APP}/migrations/ /migrations/
   ```
   Built per app:
   ```sh
   docker build --build-arg APP=orders -t ghcr.io/acme/orders-migrations:v3 .
   ```
3. Author references the resulting image in the Application CR (Phase 9.3c).

**File format constraint** — db-operator's discovery requires `<id>-<name>-apply.sql`
and `<id>-<name>-rollback.sql` pairs (numeric `id`, both files for every migration).
Content hashes are tracked; editing an applied file is a hard error at next run.
Document this prominently — it's the single largest footgun.

**Tag immutability** — `spec.sql.migrations` accepts any OCI ref, but mutable tags
(`:latest`, branch tags) cause silent skew across replicas and mask SQL changes from
the operator. Document **immutable tags or digests required**; do not enforce in CEL
(Phase 9.4-equivalent OCI digest pinning will close this loop later).

**Where this lives**

- A new section in [components/wp-operator/README.md](components/wp-operator/README.md)
  under "Application authoring", titled *"Database migrations"*, covering: file naming,
  the Dockerfile pattern above, monorepo build script, immutable-tag requirement, and
  rollback caveats (rollback is not yet wired through the platform — see Phase 9.3c
  scope notes).
- A worked example under `examples/sql-hello/migrations/` containing the seed schema
  used by the e2e test (replaces the inline seeding Job from 9.2).

#### Tasks

- [x] Add the *Database migrations* section to
  [components/wp-operator/README.md](components/wp-operator/README.md) per the design
  above.
- [x] Add `examples/sql-hello/migrations/` with at least one apply/rollback pair that
  matches the schema used by the existing 9.2 seeding Job.
- [x] Add `examples/sql-hello/migrations.Dockerfile` demonstrating the parameterised
  monorepo pattern. Build target wired into the example's Tiltfile.
- [x] Cross-reference the db-operator
  [cmd/db-migrations/spec.md](../db-operator/cmd/db-migrations/spec.md) from the new
  README section so authors can find the file-format contract upstream.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

#### Verification

`e2e-tests` resource passes (no functional change). Worked example builds cleanly via
Tilt.

---

### Phase 9.3c: Operator Migrations Integration

Wire migrations into the wp-operator: a new `spec.sql.migrations` field, a Kubernetes
Job per Application generation, an activation gate that holds traffic until the Job
succeeds, and status surfacing on failure.

**Depends on Phase 9.3a** (db-operator chart with `databaseOwner` + advisory lock).

#### Design

**CRD shape — `spec.sql.migrations`**

```yaml
spec:
  sql:
    migrations: oci://ghcr.io/acme/orders-migrations:v3   # immutable tag or @sha256:…
    users:
      - name: writer
        permissions: [...]
```

Field type: optional string. Empty / absent → no migrations Job is created (existing
9.2 behaviour). Present → Job runs before any function activation.

**Implicit `migrations` PG user**

Added unconditionally when `spec.sql.migrations` is set:

- PG username derived as `wasm_<namespace>_<app_name>_migrations` (same algorithm as
  Phase 9.2, including hash-truncation at 63 chars).
- `PostgresCredential` CR named `wasm-<namespace>-<app_name>-migrations-pg`.
- `spec.databaseOwner: true` (requires Phase 9.3a).
- `spec.permissions`: `databases: [<derived_db_name>]`, `permissions: [ALL]`.
- The name `migrations` is reserved in `spec.sql.users[*].name` (already enforced by
  the Phase 9.2 CEL rule).

**Job spec**

The wp-operator templates the Job directly (no Helm-in-operator). Job manifest:

- **Name**: `<app>-migrate-<digest12>`, where `digest12` is the first 12 hex chars of
  the SHA-256 of `spec.sql.migrations` (string hash, not OCI digest — sufficient for
  uniqueness without registry round-trips). On a re-deploy with the same migrations
  ref, the Job already exists and is reused; on a bumped tag, a new Job is created.
- **`spec.ttlSecondsAfterFinished: 86400`** — completed Jobs auto-clean after 24 h.
- **`spec.backoffLimit: 3`**, **`spec.template.spec.restartPolicy: Never`**.
- **`spec.template.spec.containers[0]`**:
  - `image`: `spec.sql.migrations` verbatim.
  - `args`: `[]` (defaults to apply-all; rollback is out of scope, see below).
  - `envFrom`: the `PostgresCredential` Secret for the migrations user
    (`PGUSER`/`PGPASSWORD`/`PGHOST`/`PGPORT`).
  - `env`: `PGDATABASE: <derived_db_name>` (overrides whatever the Secret carries —
    the credential targets one database here).

**Activation gate**

Function activation = the operator pushing `ApplicationConfig` to execution hosts via
the existing config-sync stream. The gate:

1. If `spec.sql.migrations` is set, the operator does not call
   `pushApplicationConfig` until the named Job reaches `status.succeeded >= 1`.
2. While the Job is `Active` or has not yet been created, the reconciler returns
   `RequeueAfter: 5s` and sets `Ready: False, reason: MigrationsRunning`.
3. On `status.failed > 0`, set `Ready: False, reason: MigrationFailed` with a message
   of the form `"Job <name>: pod <pod> exited with code <n>"`. Do not requeue
   automatically — the Application's owner must bump `spec.sql.migrations` (or fix the
   image and re-tag immutably) to trigger a new Job.
4. Once succeeded, the operator pushes config and `Ready: True`.

The activation gate runs *after* all `PostgresCredential`s (including the migrations
one) reach `Ready` — i.e. extends the existing 9.2 wait, not a parallel path.

**Skew between module and migrations**

Out of scope. If a user pushes a migration that introduces a breaking schema change
without updating their function modules, runtime SQL errors are the expected outcome.
Documented in 9.3b. Future Work: a single OCI artifact carrying both modules and
migrations would close this gap; not pursued now.

**Rollback**

Out of scope for 9.3c. The db-operator runner supports `--target` for forward + reverse
plans, but plumbing a target ID through the CRD adds another mutation we don't want
pre-alpha. The operator only ever runs apply-all. If a rollback is required, the user
deletes the Application (which leaves the database intact) and re-deploys against an
older migrations image — the runner's content-hash integrity check will then refuse,
forcing a manual intervention. Documented as a known limitation.

**db-operator chart pin**

The wp-operator's Helm chart depends on the db-operator chart (CRDs). Bump the
dependency pin to the version published by Phase 9.3a; record the version in
[helm/wasm-platform/Chart.yaml](helm/wasm-platform/Chart.yaml) `dependencies[]`.

#### Tasks

- [ ] **CRD**: add `Migrations *string` to `SQLSpec`. Run `make generate` in
  [components/wp-operator/](components/wp-operator/).
- [ ] **Operator — derivation**: extend the Phase 9.2 derivation utility with a
  `migrationsJobName(appName, migrationsRef)` helper (12-char digest of the ref).
- [ ] **Operator — `reconcileMigrationsCredential`**: when `spec.sql.migrations` is
  set, create the implicit migrations `PostgresCredential` with `databaseOwner: true`
  alongside the user-declared credentials.
- [ ] **Operator — `reconcileMigrationsJob`**: template and create the Job per the
  design above; idempotent on re-reconcile (Job already exists with this name → no-op).
- [ ] **Operator — activation gate**: extend the existing readiness wait to block on
  Job success; emit `MigrationsRunning` / `MigrationFailed` status reasons; format the
  failure condition message as `"Job <name>: pod <pod> exited with code <n>"`.
- [ ] **Operator — RBAC**: add `get;list;watch;create` on `batch/jobs` and `get;list`
  on `pods` (for the failed-pod name in the failure message). Update Helm chart RBAC.
- [ ] **Operator — delete path**: deletion of the Application removes the
  PostgresCredentials (already handled in 9.2); rely on TTL for completed Jobs.
- [ ] **db-operator pin**: bump the dependency in
  [helm/wasm-platform/Chart.yaml](helm/wasm-platform/Chart.yaml) to the chart version
  published by 9.3a; run `helm dependency update`.
- [ ] **`sql-hello` e2e**: replace the inline seeding Job with a real migrations image
  built from `examples/sql-hello/migrations/` (per 9.3b). Assert the Application
  reaches `Ready: True` only after the migrations Job succeeds.
- [ ] **Failure-path e2e**: a second fixture with a deliberately-broken migration
  (`SELECT * FROM nonexistent;`) asserts the Application reaches
  `Ready: False, reason: MigrationFailed` with the expected message format and that no
  function traffic is served.
- [ ] Update [components/wp-operator/README.md](components/wp-operator/README.md):
  document `spec.sql.migrations`, the implicit migrations user, the activation gate,
  failure semantics, and rollback-out-of-scope limitation.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

#### Verification

`sql-hello` e2e test passes via real migrations image. Failure-path e2e test passes.
`e2e-tests` resource passes. hello-world e2e test is unaffected.

---

### Phase 10.1: Fuel Metering + Memory Limits

Add engine-level resource limits for CPU (fuel) and memory.

#### Tasks

- [ ] Enable fuel metering on `Engine`; set fuel budget per `Store` before each invocation (`WASM_FUEL_LIMIT` env var).
- [ ] Configure `InstanceLimits` for linear memory on `Engine` (`WASM_MEMORY_LIMIT_MB` env var, default 64 MB).
- [ ] Add unit tests: a module that loops infinitely is killed with a fuel error; a module that allocates beyond the limit is killed.

#### Verification

Unit tests pass. `e2e-tests` resource passes.

---

### Phase 10.2: Wall-Clock Timeout

Add a per-invocation wall-clock timeout to cover host calls that fuel metering does not reach.

#### Tasks

- [ ] Wrap each `spawn_blocking` invocation in `tokio::time::timeout` (`WASM_TIMEOUT_SECS` env var, default 30s).
- [ ] Add a unit test: a module that sleeps longer than the timeout is cancelled and returns an error.

#### Verification

Unit test passes. `e2e-tests` resource passes.

---

### Phase 11: README Alignment

Documentation-only pass to bring all READMEs and docs into sync with the current implementation. No functional change.

#### Tasks

- [ ] Update project README status section — currently says "Phase 0 (Proof of Concept)", should reflect actual progress.
- [ ] Replace wildcard `fn.>` / `NATS_TOPIC_PREFIX` description with per-topic subscription model and internal prefix scheme.
- [ ] Replace `for_each_concurrent` reference with actual concurrency description.
- [ ] Verify module loading section matches Phase 1 implementation.
- [ ] Document gateway in execution-host README (NATS reply flow, platform JSON payload format, two-world dispatch).
- [ ] Document the two WIT worlds in the project README and `framework/` — note `message-application` for pure message-passing and `http-application` for HTTP endpoints.
- [ ] Full pass for any remaining stale claims.

#### Verification

`e2e-tests` resource passes. PR is reviewable as a docs-only change.

---

## Future Work: OCI Digest Pinning

The operator currently copies `spec.functions[].module` verbatim into `FunctionConfig.module_ref`. When a mutable tag (e.g. `:latest`) is used, different replicas may resolve different digests, updates are not detected on image push, and there is no audit trail of which digest is running.

### Tasks

- [ ] Operator resolves mutable OCI tags to immutable `sha256:` digests via the registry before pushing config to execution hosts.
- [ ] Record the resolved digest in Application status for observability.
- [ ] Re-resolve periodically (or on webhook) to detect upstream image changes and trigger a config update.
- [ ] Ensure all replicas converge on the same digest for a given generation.

---

## Future Work: Distributed Tracing (OpenTelemetry)

Add request-scoped trace propagation across component boundaries (gateway → NATS → execution host → host functions) so that a single user request can be traced end-to-end.

### Tasks

- [ ] Integrate `opentelemetry` + `tracing-opentelemetry` in Rust components; propagate trace context through NATS headers.
- [ ] Add OpenTelemetry exporter configuration (OTLP endpoint, sampling rate) as env vars.
- [ ] Inject trace/span IDs into structured log entries for log–trace correlation.

---

## Future Work: Circuit Breakers

Add circuit-breaker logic to outbound dependency calls (module cache, database pools, NATS) so that sustained failures trigger fast-fail rather than timeout accumulation.

### Tasks

- [ ] Evaluate circuit-breaker crate options (e.g. `again`, `backon`, or a thin custom wrapper).
- [ ] Apply circuit breakers to module-cache HTTP calls and database pool acquisition.
- [ ] Surface circuit state (closed/open/half-open) as a Prometheus metric.

---

## Future Work: Request-Scoped Correlation IDs

Assign a unique correlation ID to each inbound request at the gateway and propagate it through NATS headers and log entries so that all log lines for a single request can be aggregated.

### Tasks

- [ ] Generate a correlation ID at the gateway (UUID or similar) and attach it to the NATS message headers.
- [ ] Extract and attach the correlation ID as a `tracing` span field in the execution host.
- [ ] Include the correlation ID in guest log forwarding so application logs are correlated with platform logs.

---

## Future Work: Multi-Subscriber Topics

Remove the uniqueness requirement per topic. Each individual function will use a combination of its namespace, app name and function name as the ID of its consumer group, to support mutliple subscribers to the same messages. This will allow e.g. functional subscription to a message queue, but also logging / stats gathering / etc.

### Things to consider

- call-response behaviour (can secondary subscribers just not respond?)

---

## Future Work: Operator-owned databases
- wp-operator to create NATs and Redis crds
- wp-operator to create a postgres crd per namespace that needs it