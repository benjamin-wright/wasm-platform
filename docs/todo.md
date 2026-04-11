# TODO

Active implementation plan for the wasm-platform project.

---

## Execution Host: Align Implementation with README Spec

Each phase is independently launchable in its own agent session. The permanent regression guard throughout is the hello-world e2e fixture (HTTP trigger, KV counter) — it must pass at every phase boundary. After every phase, `tilt ci` must exit 0.

---

### Phase 8.1: Design Record (no code)

Make and record all design decisions required before CRD and proto changes can begin. No implementation. This phase is complete when all decisions are captured in `docs/architecture.md`.

#### Tasks

- [x] Draft multi-function Application CRD schema — `spec.functions` list with per-function `name`, `module` (OCI ref), and `trigger` (http or topic); assess backwards compatibility with existing single-module CRs.
- [x] Draft `spec.metrics` schema — `{name, type, labels}` list; decide validation rules (name format, max label count, reserved names).
- [x] Decide operator uniqueness enforcement strategy for metric names — admission webhook vs. reconciler-time validation; assess failure UX for each.
- [x] Decide migrations contract — how the migrations image is referenced in the spec, what the operator does on first apply vs. upgrade, and what the failure/rollback model is.
- [x] Assess config-sync proto changes needed — per-function module refs and triggers grouped by application, plus `MetricDefinition` repeated field.
- [x] Record all decisions in a Decisions block in `docs/architecture.md`.

#### Verification

PR review only — no functional change, so `tilt ci` is not a signal here.

---

### Phase 8.2: Multi-Function CRD + Config-Sync Proto ✅

Migrate the Application CRD from a single-module shape to a `spec.functions` list and propagate that shape through the config-sync proto, operator, and execution host.

#### Tasks

- [x] Update Go `Application` CRD types to `spec.functions` (list of `{name, module, trigger}`).
- [x] Regenerate CRD manifests (`make generate` in `components/wp-operator/`).
- [x] Update config-sync proto: per-function module refs and triggers grouped under an application context.
- [x] Update operator reconciler to push per-function config using the new proto shape.
- [x] Update execution host to receive and index per-function config.
- [x] Migrate the hello-world Application CR to the new `spec.functions` single-entry shape.
- [x] Migrate the message-counter Application CR to the new `spec.functions` single-entry shape.
- [x] Update wp-operator README to document new CRD shape.

#### Verification

`tilt ci` passes — hello-world continues to work under the new CRD shape.

---

### Phase 8.3: Graceful Shutdown & NATS Drain

Implement true graceful drain on SIGTERM so that in-flight WASM invocations complete before the process exits, and the pod leaves its NATS queue groups cleanly before Kubernetes sends SIGKILL.

#### Design

- A `shutdown` broadcast channel fires when SIGTERM is received.
- `manage_nats_subscriptions` listens for shutdown and drops all subscriptions (sends UNSUB to NATS, removes the replica from every queue group).
- Dropping the subscriptions closes the per-topic forwarding tasks, which close the senders on `msg_tx`, which causes `msg_rx` in `process_nats_messages` to drain and return naturally.
- Replace the fire-and-forget `tokio::spawn` per invocation with a `tokio::task::JoinSet` so the message loop can `.join_all()` after `msg_rx` closes.
- `main` awaits the drain to complete, then exits — Kubernetes sees a clean process exit within the termination grace period.

#### Tasks

- [ ] Add a `shutdown` `tokio::sync::broadcast` channel in `main.rs` and install a SIGTERM handler that sends on it.
- [ ] Pass the shutdown receiver into `manage_nats_subscriptions`; on signal, clear all subscriptions and return.
- [ ] Pass the shutdown receiver into `process_nats_messages`; switch per-invocation spawns to a `JoinSet`; after `msg_rx` closes, `join_all()` the set before returning.
- [ ] Update `main`'s `tokio::select!` to await the `process_nats_messages` future (which now drains and returns) rather than racing it against the health server.
- [ ] Update `components/execution-host/README.md` to document the shutdown sequence.

#### Verification

`tilt ci` passes. Manual test: trigger an invocation, immediately `kubectl delete pod` the execution-host pod, confirm the invocation completes and the counter increments exactly once with no duplicate.

---

### Phase 8.5: Metrics CRD Schema + Operator Validation

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

### Phase 8.6: Host Metrics Implementation

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
