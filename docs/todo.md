# TODO

Active implementation plan for the wasm-platform project.

---

## Execution Host: Align Implementation with README Spec

Each phase is independently launchable in its own agent session. The permanent regression guard throughout is the hello-world e2e fixture (HTTP trigger, KV counter) — it must pass at every phase boundary. After every phase, `tilt ci` must exit 0.

---

### Phase 8.1: Design Record (no code)

Make and record all design decisions required before CRD and proto changes can begin. No implementation. This phase is complete when all decisions are captured in `docs/architecture.md`.

#### Tasks

- [ ] Draft multi-function Application CRD schema — `spec.functions` list with per-function `name`, `module` (OCI ref), and `trigger` (http or topic); assess backwards compatibility with existing single-module CRs.
- [ ] Draft `spec.metrics` schema — `{name, type, labels}` list; decide validation rules (name format, max label count, reserved names).
- [ ] Decide operator uniqueness enforcement strategy for metric names — admission webhook vs. reconciler-time validation; assess failure UX for each.
- [ ] Decide migrations contract — how the migrations image is referenced in the spec, what the operator does on first apply vs. upgrade, and what the failure/rollback model is.
- [ ] Assess config-sync proto changes needed — per-function module refs and triggers grouped by application, plus `MetricDefinition` repeated field.
- [ ] Record all decisions in a Decisions block in `docs/architecture.md`.

#### Verification

PR review only — no functional change, so `tilt ci` is not a signal here.

---

### Phase 8.2: Multi-Function CRD + Config-Sync Proto

Migrate the Application CRD from a single-module shape to a `spec.functions` list and propagate that shape through the config-sync proto, operator, and execution host.

#### Tasks

- [ ] Update Go `Application` CRD types to `spec.functions` (list of `{name, module, trigger}`).
- [ ] Regenerate CRD manifests (`make generate` in `components/wp-operator/`).
- [ ] Update config-sync proto: per-function module refs and triggers grouped under an application context.
- [ ] Update operator reconciler to push per-function config using the new proto shape.
- [ ] Update execution host to receive and index per-function config.
- [ ] Migrate the hello-world Application CR to the new `spec.functions` single-entry shape.

#### Verification

`tilt ci` passes — hello-world continues to work under the new CRD shape.

---

### Phase 8.3: Metrics CRD Schema + Operator Validation

Add `spec.metrics` to the Application CR, enforce cluster-wide uniqueness in the operator, and forward metric definitions to execution hosts (no host registration yet).

#### Tasks

- [ ] Add `spec.metrics` (`{name, type, labels}`) to the Application CRD types and regenerate manifests.
- [ ] Add operator reconciler logic to reject Applications whose metric names collide with an existing Application's names (reconciler-time validation, per the Phase 8.1 decision).
- [ ] Extend config-sync proto with a `MetricDefinition` repeated field.
- [ ] Update operator to include metric definitions in the config pushed to execution hosts.
- [ ] Update execution host to receive metric definitions — store in config and log on receipt; do **not** register them yet.
- [ ] Add a `spec.metrics` entry to the hello-world Application CR.

#### Verification

Deploying two Applications that claim the same metric name causes the second one to enter an error status. `tilt ci` passes.

---

### Phase 8.4: Host Metrics Implementation

Pre-register metrics on config arrival, wire the `counter-increment` WIT host function, and expose `/metrics`.

#### Tasks

- [ ] Host pre-registers `CounterVec`/`GaugeVec` from received metric definitions on config arrival.
- [ ] Implement the `counter-increment` WIT host function — validate call against declared schema, drop calls with unexpected label keys.
- [ ] Expose a `/metrics` Prometheus endpoint; add `app_name` and `app_namespace` labels to all series.
- [ ] Add a `message-application` example module that increments a counter on each invocation.
- [ ] Add an e2e test that hits `/metrics` after invoking the example module and asserts the counter has advanced.

#### Verification

New e2e test passes demonstrating guest → host metric increment. `tilt ci` passes.

---

### Phase 9.1: SQL Host Function (No Migrations)

Implement the `sql` WIT interface host functions backed by a per-app PostgreSQL connection pool, without any migrations machinery.

#### Tasks

- [ ] Implement `sql.query` and `sql.execute` WIT host functions.
- [ ] Per-app connection pool keyed by `(database_name, username)`, lazily initialised from config-sync credentials.
- [ ] Add a `sql-hello` example module that reads from a pre-seeded fixture table.
- [ ] Add an e2e fixture: deploy the `sql-hello` Application with a pre-seeded table created via a Kubernetes Job; assert the HTTP response includes data from the table.

#### Verification

New e2e test passes. `tilt ci` passes. hello-world e2e test is unaffected.

---

### Phase 9.2: Migrations Job Lifecycle

Add migrations support to the operator: create a Job on Application create/upgrade, gate activation on Job success, and surface failure in Application status.

#### Tasks

- [ ] Operator creates a Kubernetes Job running the migrations image on Application create/upgrade (per Phase 8.1 migrations contract).
- [ ] Activation gate: no function receives traffic until the migrations Job completes successfully.
- [ ] Surface `MigrationFailed` in Application status when the Job fails.
- [ ] Extend the `sql-hello` e2e fixture to use a migrations Job rather than a pre-seeded table.

#### Verification

e2e test exercises the full migrations-then-activate flow. `tilt ci` passes.

---

### Phase 10.1: Fuel Metering + Memory Limits

Add engine-level resource limits for CPU (fuel) and memory.

#### Tasks

- [ ] Enable fuel metering on `Engine`; set fuel budget per `Store` before each invocation (`WASM_FUEL_LIMIT` env var).
- [ ] Configure `InstanceLimits` for linear memory on `Engine` (`WASM_MEMORY_LIMIT_MB` env var, default 64 MB).
- [ ] Add unit tests: a module that loops infinitely is killed with a fuel error; a module that allocates beyond the limit is killed.

#### Verification

Unit tests pass. `tilt ci` passes.

---

### Phase 10.2: Wall-Clock Timeout

Add a per-invocation wall-clock timeout to cover host calls that fuel metering does not reach.

#### Tasks

- [ ] Wrap each `spawn_blocking` invocation in `tokio::time::timeout` (`WASM_TIMEOUT_SECS` env var, default 30s).
- [ ] Add a unit test: a module that sleeps longer than the timeout is cancelled and returns an error.

#### Verification

Unit test passes. `tilt ci` passes.

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

`tilt ci` passes. PR is reviewable as a docs-only change.
