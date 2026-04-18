# TODO

Active implementation plan for the wasm-platform project.

---

## Execution Host: Align Implementation with README Spec

Each phase is independently launchable in its own agent session. The permanent regression guard throughout is the hello-world e2e fixture (HTTP trigger, KV counter) — it must pass at every phase boundary. After every phase, the `e2e-tests` resource must pass (trigger via the Tilt MCP server).

---

### Phase 9.1: KV Implicit Isolation (Drop `keyValue` field and `store` parameter)

Make the KV host functions implicitly available to every Application with isolation derived from `(namespace, app)` rather than a user-supplied prefix. Simultaneously drop the `store` parameter from the `kv` WIT interface — once the prefix is automatic, `store` is a redundant guest-controlled sub-namespace that the app can implement itself by prefixing its keys.

**Why:** the current `spec.keyValue: string` field looks meaningful but enforces nothing — two apps in the same namespace can silently share a prefix and corrupt each other's data. Replacing it with an automatic `<namespace>/<app>/` prefix is both safer and removes a CRD field, an operator reconcile branch, a proto message, and a per-function config field. Apps that don't use KV are unaffected; apps that do get the same surface minus the noise. KV requires no provisioning (the execution host already holds a multiplexed Redis connection), so there is no reason to make it opt-in.

**Pre-1.0:** there is no production data and no migration concern. Existing keys under the old prefix scheme can be abandoned.

#### Context for implementing agents

The relevant files (read these before changing anything):

- WIT contract — [framework/runtime.wit](../framework/runtime.wit) (the `kv` interface and both worlds import it).
- CRD types — [components/wp-operator/api/v1alpha1/application_types.go](../components/wp-operator/api/v1alpha1/application_types.go) (`ApplicationSpec.KeyValue`).
- Generated CRD manifests — [helm/wasm-platform/](../helm/wasm-platform/) and any `config/crd` output of `make manifests`.
- Operator reconciler — [components/wp-operator/internal/controller/application_controller.go](../components/wp-operator/internal/controller/application_controller.go) (`reconcileKVBinding`, the `if app.Spec.KeyValue != ""` branch, `RedisSecretName`/`RedisSecretNamespace` config knobs).
- ConfigSync proto — [proto/configsync/](../proto/configsync/) (the `KeyValueConfig` message and `ApplicationConfig.key_value` field). Regenerate Go bindings under [components/wp-operator/internal/grpc/configsync/](../components/wp-operator/internal/grpc/configsync/) and Rust bindings under [components/execution-host/](../components/execution-host/) (look for `build.rs` / `tonic-build`).
- Execution host config registry — [components/execution-host/src/config.rs](../components/execution-host/src/config.rs) (`FunctionEntry.key_value`).
- Execution host runtime — [components/execution-host/src/runtime.rs](../components/execution-host/src/runtime.rs) (`HostState.kv_prefix`; `app_name` and `app_namespace` already exist on `HostState`).
- KV host functions — [components/execution-host/src/host_kv.rs](../components/execution-host/src/host_kv.rs) (currently formats `"{kv_prefix}/{store}/{key}"`).
- Helm chart for the operator — Redis Secret RBAC and env wiring; the operator no longer needs to read any Redis Secret. The execution host's `REDIS_URL` env stays exactly as it is.
- Demo app manifest — [examples/demo-app/k8s/application.yaml](../examples/demo-app/k8s/application.yaml) (`spec.keyValue: demo-app`).
- Architecture doc — [docs/architecture.md](../docs/architecture.md) (Data Layer table row for Redis, and the "Connection model" paragraph mentioning the per-app key-prefix delta).
- Component READMEs — [components/wp-operator/README.md](../components/wp-operator/README.md), [components/execution-host/README.md](../components/execution-host/README.md).
- Demo app guest code — [examples/demo-app/](../examples/demo-app/) (any guest call sites that pass a `store` argument to `kv.get`/`kv.set`/etc., e.g. `demo-app-http`, `demo-app-messages`).
- e2e tests — [tests/e2e/](../tests/e2e/) and the hello-world fixture (HTTP + KV counter).

**New isolation scheme:** `format!("{namespace}/{app}/{key}")`. Note this is one segment shorter than today (today: `<ns>/<spec.keyValue>/<store>/<key>`).

**Failure mode preserved:** when `REDIS_URL` is not configured on the execution host, KV calls return `Err("KV host function unavailable: REDIS_URL not configured")`. Apps that never call KV are unaffected. Do not silently no-op.

#### Tasks

Do these in order — step 1 breaks compilation and the type system guides the rest.

- [x] **WIT.** In `framework/runtime.wit`, remove the `store: string` parameter from every function in `interface kv` (`get`, `set`, `delete`, `get-int`, `set-int`, `incr`, `decr`). Both `message-application` and `http-application` worlds already import `kv` — no world changes needed.
- [x] **Execution host KV.** In `host_kv.rs`, drop the `store` parameter from every function and change the key construction to `format!("{}/{}/{}", self.app_namespace, self.app_name, key)`. Drop `HostState.kv_prefix` from `runtime.rs` (the field, all sites that set it, and any constructor argument). The `app_name` / `app_namespace` fields on `HostState` already provide what's needed.
- [x] **Execution host config.** In `config.rs`, remove `FunctionEntry.key_value` and any propagation of `KeyValueConfig`. Audit anywhere that destructures or populates this field.
- [x] **Proto.** In `proto/configsync/`, delete the `KeyValueConfig` message and the `key_value` field on `ApplicationConfig`. Regenerate Go and Rust bindings via the existing `make` target (check `Makefile` for the proto-gen target — do not hand-edit `*.pb.go` files).
- [x] **Operator.** In `application_controller.go`, delete `reconcileKVBinding`, the `if app.Spec.KeyValue != ""` branch in `Reconcile`, the `RedisSecretName` / `RedisSecretNamespace` fields on the controller's `Config` struct, and any wiring that reads them (likely in the operator's `main.go` and Helm values).
- [x] **CRD.** In `application_types.go`, remove the `KeyValue string` field. Run `make manifests` (or equivalent) to regenerate CRD YAML; do not hand-edit generated CRD manifests.
- [x] **Helm.** In the operator's Helm chart under [helm/wasm-platform/](../helm/wasm-platform/), remove the Redis Secret reference / RBAC for the operator (the operator no longer reads a Redis Secret). Leave the execution host's `REDIS_URL` env wiring untouched.
- [x] **Demo app manifest.** In `examples/demo-app/k8s/application.yaml`, remove the `keyValue: demo-app` line.
- [x] **Demo app guest code.** Update any `kv.*` call sites in [examples/demo-app/](../examples/demo-app/) and [examples/counter-app/](../examples/counter-app/) to drop the `store` argument. Regenerate WIT bindings for each guest as appropriate.
- [ ] **Tests.** Delete operator unit tests for `reconcileKVBinding`. Update host KV unit tests to assert the prefix is derived from injected `(namespace, app)` and not from any config field. Update guest-side tests to match the new WIT signature.
- [x] **Architecture doc.** In `docs/architecture.md`, update the Data Layer table row for Redis to: "Single shared instance. Per-app key-prefix isolation (`<namespace>/<app>/`), assigned automatically — no CRD field required." In the "Connection model" paragraph, remove the clause about Redis carrying a per-app key prefix in the config delta.
- [x] **Component READMEs.** Update [components/wp-operator/README.md](../components/wp-operator/README.md) and [components/execution-host/README.md](../components/execution-host/README.md) — remove KV-binding sections from the operator README; update the execution-host README to describe automatic `<namespace>/<app>/` prefixing and the dropped `store` parameter.
- [x] **WIT signature in docs.** If any doc embeds the `kv` interface signature, update it.
- [ ] **e2e.** Run the hello-world fixture and the `e2e-tests` resource (via the Tilt MCP server). The hello-world KV counter should still work after the contract change.

#### Verification

hello-world e2e fixture passes (HTTP + KV counter). `e2e-tests` resource passes. The Application CRD no longer has a `keyValue` field. The `ApplicationConfig` proto no longer has a `key_value` field. The `kv` WIT interface no longer has a `store` parameter. The operator does not read any Redis Secret.

---

### Phase 9.2: SQL Host Function (No Migrations)

Implement the `sql` WIT interface host functions backed by a per-app PostgreSQL connection pool, without any migrations machinery.

#### Tasks

- [ ] Implement `sql.query` and `sql.execute` WIT host functions.
- [ ] Per-app connection pool keyed by `(database_name, username)`, lazily initialised from config-sync credentials.
- [ ] Add a `sql-hello` example module that reads from a pre-seeded fixture table.
- [ ] Add an e2e fixture: deploy the `sql-hello` Application with a pre-seeded table created via a Kubernetes Job; assert the HTTP response includes data from the table.

#### Verification

New e2e test passes. `e2e-tests` resource passes. hello-world e2e test is unaffected.

---

### Phase 9.3: Migrations Job Lifecycle

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
