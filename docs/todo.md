# TODO

Active implementation plan for the wasm-platform project.

---

## Execution Host: Align Implementation with README Spec

### Phase 8: Application CRD Redesign + Metrics

#### Design

The Application CRD needs a structural redesign before SQL and metrics can be implemented. Two concerns are bundled here because both require CRD changes, config-sync proto changes, and operator work — doing them in one pass avoids a second CRD migration.

**Multi-function shape:** should a single Application CR support multiple WASM functions, each its own module, all sharing one PostgreSQL database and one Redis key-prefix? The current shape is one-module-per-CR. A multi-function model — `spec.functions` as a list of `{name, module, trigger}` entries — has significant implications: isolation is at the application boundary; DB migrations must complete before any function is activated; config-sync proto must carry per-function entries grouped under an application context.

**CRD-declared metrics:** `spec.metrics` carries a list of `{name, type: counter|gauge, labels: [string]}` entries. The operator validates uniqueness across all Applications (no two apps may claim the same metric name) and rejects CRDs that would collide. Definitions are pushed to execution hosts via config-sync. The host pre-registers `CounterVec`/`GaugeVec` on config arrival — no lazy registration, no schema drift. A label schema change requires a CRD update and redeploy, making it intentional. Guests call `counter-increment(name, value, labels: list<tuple<string,string>>)` — the host validates the call against the declared schema and drops calls with unexpected keys. The `/metrics` endpoint exposes all series with `app_name`/`app_namespace` labels added by the host. Low-cardinality labels are a documented constraint; there is no per-metric series cap in this iteration.

This phase produces decisions and a revised CRD schema. No host implementation of metrics or SQL. Phase 9 (SQL) is blocked on this phase completing.

#### Tasks

- [ ] Draft multi-function Application CRD schema — `spec.functions` list with per-function `name`, `module` (OCI ref), and `trigger` (http or topic); assess backwards compatibility with existing single-module CRs.
- [ ] Draft `spec.metrics` schema — `{name, type, labels}` list; decide validation rules (name format, max label count, reserved names).
- [ ] Decide operator uniqueness enforcement strategy for metric names — admission webhook vs. reconciler-time validation; assess failure UX for each.
- [ ] Decide migrations contract — how the migrations image is referenced in the spec, what the operator does on first apply vs. upgrade, and what the failure/rollback model is.
- [ ] Assess config sync proto changes — per-function module refs and triggers, plus `MetricDefinition` repeated field; update `proto/configsync` design.
- [ ] Record all decisions in the Decisions block and update `docs/architecture.md` before closing this phase.

### Phase 9: SQL Host Function + Migrations

#### Design

Deferred — expand once Phase 8 design decisions are recorded. At minimum this phase will cover: `sql` WIT interface host implementation, per-application connection pool keyed by `(database_name, username)`, migrations Job lifecycle managed by the wp-operator, and an e2e fixture exercising a SQL-backed WASM module.

#### Tasks

- [ ] TBD — expand after Phase 8 decisions are finalised.

### Phase 10: Sandboxing & Resource Limits

#### Tasks

- [ ] Fuel metering — enable on `Engine`, set budget per `Store` before `on-message` (configurable via `WASM_FUEL_LIMIT`).
- [ ] Memory limits — configure `InstanceLimits` on `Engine` (e.g. 64 MB default, `WASM_MEMORY_LIMIT_MB`).
- [ ] Wall-clock timeout — wrap `spawn_blocking` in `tokio::time::timeout` (e.g. 30s default, `WASM_TIMEOUT_SECS`).
- [ ] Instance pooling (stretch goal) — `PoolingAllocationConfig` for pre-allocated slots; defer if per-invocation model performs adequately.

### Phase 11: README Alignment

#### Tasks

- [ ] Update project README status section — currently says "Phase 0 (Proof of Concept)", should reflect actual progress.
- [ ] Replace wildcard `fn.>` / `NATS_TOPIC_PREFIX` description with per-topic subscription model and internal prefix scheme.
- [ ] Replace `for_each_concurrent` reference with actual concurrency description.
- [ ] Verify module loading section matches Phase 1 implementation.
- [ ] Document gateway in execution-host README (NATS reply flow, platform JSON payload format, two-world dispatch).
- [ ] Document the two WIT worlds in the project README and `framework/` — note `message-application` for pure message-passing and `http-application` for HTTP endpoints.
- [ ] Full pass for any remaining stale claims.

### Verification

After every phase, run `tilt ci` to confirm the full stack deploys and the end-to-end test passes. A phase is not complete until `tilt ci` exits 0.
