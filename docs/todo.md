# TODO

Active implementation plan for the wasm-platform project.

---

## Execution Host: Align Implementation with README Spec

### Decisions

- **NATS:** per-topic subscriptions (current code) are correct; README will be updated.
- **Concurrency:** semaphore approach is kept (functionally equivalent to `for_each_concurrent`).
- **Instance pooling:** deferred as stretch goal.
- **PG/Redis connections:** pool-per-app, lazily initialized, shared across invocations — not per-invocation.
- **Internal topic prefixing:** all topics are prefixed by the platform to prevent cross-concern collisions. Topic-only apps use prefix `fn.` (e.g. user writes `my-app.events` → NATS subject `fn.my-app.events`). HTTP apps get an auto-generated topic `http.<namespace>.<name>`. This is invisible to the platform user and enforced by the wp-operator before pushing config to execution hosts.
- **Topic uniqueness:** still cluster-wide, still based on the user-supplied `spec.topic` value (before prefixing). The `fn.` vs `http.` prefix guarantees no collision between trigger types, so the uniqueness check only needs to compare within the same prefix class. No additional validation is needed to ban user-supplied topics starting with `fn.` or `http.` — the operator always prepends `fn.` to topic-only apps, so a user entering `http.whatever` results in the NATS subject `fn.http.whatever`, which cannot collide with a genuine HTTP app's `http.<ns>.<name>` subject.
- **WIT worlds:** `framework/runtime.wit` is split into two worlds. `world message-application` retains the existing `on-message` binary payload export (renamed from `world application`). `world http-application` exports `on-request` with typed `http-request` / `http-response` records — module authors get a clean interface with no manual parsing. The WASI HTTP resource model is explicitly not used; custom records are simpler to implement on the host and sufficient for a buffered FaaS model.
- **Gateway language:** Rust (consistent with execution host, avoids a Go dependency for a non-operator component).
- **Gateway route discovery:** wp-operator pushes route config to the gateway via gRPC (reuses the config-sync pattern, keeps CRD watching centralised in the operator).
- **HTTP transport (gateway ↔ execution host):** the gateway serialises the incoming HTTP request as a platform-private JSON object for the NATS payload. The execution host decodes this and calls `on-request` with properly typed WIT records — the module never sees JSON. The execution host serialises the returned `HttpResponse` record back to JSON for the NATS reply. This is an internal platform format; a future auth middleware layer can inject a user ID by adding an `x-user-id` entry to the headers map before the NATS publish.
- **E2E test approach:** Go module at `tests/e2e/` with `//go:build integration` tag. Traefik `Ingress` routes `localhost:80` to the gateway (no TLS, configurable host, added to gateway Helm chart). The hello-world Application CR (HTTP trigger, KV counter) is the permanent test fixture. The e2e `Tiltfile` runs `go test -tags integration -count=1 -v ./...` as a `local_resource` with a dep on `hello-world`, ensuring it runs in `tilt ci`.
- **Logging interface:** a custom WIT `log` interface is preferred over wiring WASI stdout/stderr. Guests use explicit log levels; the host forwards to `tracing` with per-app labels. `log::emit` is fire-and-forget — no error return — to keep guest code minimal. Implemented: `interface log` in `framework/runtime.wit`, `host_log.rs` in execution-host, `app_name`/`app_namespace` fields on `HostState`.
- **Metrics interface:** two functions (`counter-increment`, `gauge-set`) cover instrumentation needs without over-designing. The host owns all Prometheus labelling; guests supply only a name and value. Metrics are aggregated in-process per execution-host pod and exposed on a `/metrics` scrape endpoint — per-app attribution comes from `app_name`/`app_namespace` labels, not separate endpoints.
- **SQL & Application CRD structure:** SQL implementation is deferred to Phase 9, pending Phase 8 design decisions. Direction under consideration: multi-function Application CRD where `spec.functions` is a list of `{name, module, trigger}` entries; one PostgreSQL database and one Redis key-prefix per Application, shared across all functions. DB migrations are a prerequisite for SQL to be useful — a migrations step (operator-managed run-to-completion Job, image reference in the Application spec) must complete before any function is activated.

---

### Phase 7b: Metrics Interface

#### Design

Add a `metrics` interface to `framework/runtime.wit` and import it into both worlds. Two functions cover the primary instrumentation needs: `counter-increment` (monotonically increasing count, e.g. requests handled, errors) and `gauge-set` (point-in-time value, e.g. queue depth, current connections). Guests supply a metric name and a `u64` value; the host owns all labelling and aggregation.

The execution host maintains an in-process Prometheus registry (via the `prometheus` crate). Each call is recorded against a `CounterVec` or `GaugeVec` keyed by metric name, with labels `app_name` and `app_namespace` added automatically. The existing `/metrics` Prometheus scrape endpoint (or a new one) exposes the aggregated data. This means a single scrape target captures metrics from every WASM app running on that host, labelled for per-app filtering.

#### Tasks

- [ ] Add `interface metrics` to `framework/runtime.wit` — `counter-increment: func(name: string, value: u64)` and `gauge-set: func(name: string, value: u64)` — import into both worlds.
- [ ] Add `prometheus` and `prometheus-hyper` (or `axum`-based) dependencies to `components/execution-host/Cargo.toml`.
- [ ] Implement `src/host_metrics.rs` — maintain a `prometheus::Registry` in `RuntimeState`; `CounterVec` and `GaugeVec` keyed by `(metric_name, app_name, app_namespace)`. Lazily register new metric names on first use.
- [ ] Expose a `/metrics` HTTP endpoint in execution-host — serve the Prometheus text format from the registry.
- [ ] Update `HostState` — pass `Arc<MetricsRegistry>` and per-invocation app labels so `host_metrics.rs` can record calls.
- [ ] Wire `metrics` into `Linker` — call `metrics::add_to_linker` for both worlds in `RuntimeState::new()`.
- [ ] Update hello-world example — call `metrics::counter_increment("requests_total", 1)` per invocation to exercise the interface; update `examples/hello-world/README.md`.
- [ ] Add Prometheus scrape config to the execution-host Helm chart.
- [ ] Update `framework/runtime.wit` documentation and affected component READMEs.

### Phase 8: Multi-function Application CRD Design

#### Design

Before implementing SQL, the Application CRD structure needs a design decision: should a single Application CR support multiple WASM functions, each its own module, all sharing one PostgreSQL database and one Redis key-prefix scoped to the application?

The current shape is one-module-per-CR. A multi-function model — `spec.functions` as a list of `{name, module, trigger}` entries — has significant implications:
- The wp-operator manages one database and one Redis prefix per Application, regardless of function count. Isolation is at the application boundary, not the function boundary.
- DB migrations are a prerequisite for SQL to be useful. A migrations step must complete before any function is activated. The Application spec must reference a migrations image; the wp-operator manages the run-to-completion Job lifecycle, including failure and rollback.
- Config sync, NATS subscription management, and module loading in the execution host become per-function entries grouped under an Application context.
- The config sync proto must carry per-function module refs and triggers alongside per-application credentials.

This phase produces decisions and a revised Application CRD schema. No host implementation changes. Phase 9 (SQL) is blocked on this phase completing.

#### Tasks

- [ ] Draft multi-function Application CRD schema — `spec.functions` list with per-function `name`, `module` (OCI ref), and `trigger` (http or topic); assess backwards compatibility with existing single-module CRs.
- [ ] Decide migrations contract — how the migrations image is referenced in the spec, what the operator does on first apply vs. upgrade, and what the failure/rollback model is.
- [ ] Assess config sync proto changes — how the execution host receives per-application config with multiple function entries; update `proto/configsync` design if needed.
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
