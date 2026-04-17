# TODO

Active implementation plan for the wasm-platform project.

---

## Execution Host: Align Implementation with README Spec

Each phase is independently launchable in its own agent session. The permanent regression guard throughout is the hello-world e2e fixture (HTTP trigger, KV counter) — it must pass at every phase boundary. After every phase, the `e2e-tests` resource must pass (trigger via the Tilt MCP server).

---

### Phase 8.6: Host Metrics Implementation

Pre-register user-defined metrics on config arrival, wire the `counter-increment` WIT host function, instrument the execution host with built-in platform metrics, and expose `/metrics`.

#### Design

Two classes of metrics are exposed on `/metrics`:

- **User-defined** — registered from `spec.metrics` on config arrival; incremented by guests via the `counter-increment` WIT host function. Labelled with `app_name` and `app_namespace` (host-injected).
- **Platform** — fixed metrics emitted by the host itself, independent of any Application spec. Labelled with `app_name` and `app_namespace` where per-app attribution is meaningful.

Platform metrics to implement:

| Metric | Type | Labels | Description |
|---|---|---|---|
| `wasm_module_compilations_total` | Counter | `app_name`, `app_namespace`, `result` (`ok`/`err`) | AOT compilations triggered on config arrival. |
| `wasm_events_received_total` | Counter | `app_name`, `app_namespace`, `trigger` (`http`/`topic`) | Invocation requests received (before dispatch). |
| `wasm_messages_sent_total` | Counter | `app_name`, `app_namespace` | `messaging.send` host function calls. |
| `wasm_kv_reads_total` | Counter | `app_name`, `app_namespace` | `kv.get` host function calls. |
| `wasm_kv_writes_total` | Counter | `app_name`, `app_namespace` | `kv.set` / `kv.delete` host function calls. |
| `wasm_http_requests_received_total` | Counter | `app_name`, `app_namespace`, `status` (HTTP status code) | HTTP invocations completed; status is the guest's response code. |

#### Tasks

- [ ] Expose a `/metrics` Prometheus endpoint.
- [ ] Host pre-registers `CounterVec`/`GaugeVec` from received metric definitions on config arrival; add `app_name` and `app_namespace` labels to all user-defined series.
- [ ] Implement the `counter-increment` WIT host function — validate call against declared schema, drop calls with unexpected label keys.
- [ ] Register and increment `wasm_module_compilations_total` on each AOT compilation attempt.
- [ ] Register and increment `wasm_events_received_total` on each invocation dispatch (HTTP and topic triggers).
- [ ] Register and increment `wasm_messages_sent_total` on each `messaging.send` host function call.
- [ ] Register and increment `wasm_kv_reads_total` and `wasm_kv_writes_total` on the corresponding KV host function calls.
- [ ] Register and increment `wasm_http_requests_received_total` (labelled by guest response status) on each completed HTTP invocation.
- [ ] Add a `message-application` example module that increments a user-defined counter on each invocation.
- [ ] Add an e2e test that hits `/metrics` after invoking the example module and asserts both the user-defined counter and at least one platform counter (`wasm_events_received_total`) have advanced.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

#### Verification

New e2e test passes demonstrating guest → host metric increment and platform metric emission. `e2e-tests` resource passes.

---

### Phase 9.1: SQL Host Function (No Migrations)

Implement the `sql` WIT interface host functions backed by a per-app PostgreSQL connection pool, without any migrations machinery.

#### Tasks

- [ ] Implement `sql.query` and `sql.execute` WIT host functions.
- [ ] Per-app connection pool keyed by `(database_name, username)`, lazily initialised from config-sync credentials.
- [ ] Add a `sql-hello` example module that reads from a pre-seeded fixture table.
- [ ] Add an e2e fixture: deploy the `sql-hello` Application with a pre-seeded table created via a Kubernetes Job; assert the HTTP response includes data from the table.

#### Verification

New e2e test passes. `e2e-tests` resource passes. hello-world e2e test is unaffected.

---

### Phase 9.2: Migrations Job Lifecycle

Add migrations support to the operator: create a Job on Application create/upgrade, gate activation on Job success, and surface failure in Application status.

#### Tasks

- [ ] Operator creates a Kubernetes Job running the migrations image on Application create/upgrade (per Phase 8.1 migrations contract).
- [ ] Activation gate: no function receives traffic until the migrations Job completes successfully.
- [ ] Surface `MigrationFailed` in Application status when the Job fails.
- [ ] Extend the `sql-hello` e2e fixture to use a migrations Job rather than a pre-seeded table.

#### Verification

e2e test exercises the full migrations-then-activate flow. `e2e-tests` resource passes.

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
