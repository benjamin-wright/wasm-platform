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

---

### Phase 6: End-to-End Test

Adds a Kubernetes Ingress for the gateway and a Go-based end-to-end test suite that runs from the host machine against the deployed platform. This is the verification gate for all subsequent work — `tilt ci` must pass before a phase is considered complete.

#### Design

- **Ingress:** a Traefik `Ingress` resource routes external HTTP traffic on `localhost:80` (already mapped by k3d's `-p "80:80@loadbalancer"`) to the gateway `ClusterIP` Service. The Ingress is added to the gateway Helm chart with a configurable host (default: empty, meaning match all). No TLS.
- **Test runner:** a Go module at `tests/e2e/` with `//go:build integration` tag. Uses `net/http` to send requests to `http://localhost/hello` (via the Traefik ingress). Asserts the response status, body content, and that the KV counter increments across calls.
- **Tilt integration:** a `tests/e2e/Tiltfile` defines a `local_resource` that runs `go test -tags integration -v ./...` from `tests/e2e/`. It depends on `hello-world` (ensuring the full stack is deployed) and has `auto_init=True` / default `trigger_mode` so it runs in `tilt ci`.
- **hello-world as test fixture:** the existing hello-world Application CR (HTTP trigger, KV counter) is the test target. No new WASM module is needed.

#### Tasks

- [x] Add Ingress resource to gateway Helm chart (`components/gateway/helm/templates/ingress.yaml`) — route `/*` to the gateway Service on its HTTP port.  Update `values.yaml` with `ingress.enabled: true`.
- [x] Create `tests/e2e/` Go module — `go.mod`, `e2e_test.go` with `//go:build integration`. Test: `GET http://localhost/hello` twice, assert 200 status, parse `requests=N` from body, assert second N > first N.
- [x] Create `tests/e2e/Tiltfile` — `local_resource('e2e-tests', cmd='cd tests/e2e && go test -tags integration -count=1 -v ./...', resource_deps=['hello-world'])`. Default `auto_init` and `trigger_mode` so it runs in `tilt ci`.
- [x] Load `tests/e2e/Tiltfile` from root `Tiltfile` — `load('./tests/e2e/Tiltfile', 'e2e_tests')`, call `e2e_tests()`.
- [x] Verify `tilt ci` passes end-to-end.

---

### Phase 7a: SQL Host Function

Depends on Phase 1 (per-app routing provides the `ApplicationConfig` at invocation time).

- **Connection model:** the execution host is configured once with the shared PostgreSQL host and port (`PG_HOST`, `PG_PORT` env vars). Per-app credentials (database name, username, password) arrive via `SqlConfig` in the `ConfigSync` stream. The host builds connection strings internally by combining the shared host/port with the per-app credentials. Connection pools are per-application, keyed by `(database_name, username)`, lazily initialized, and shared across invocations.

- [ ] Implement `sql` interface in new `src/host_sql.rs` — `query`/`execute` backed by `tokio-postgres`. Build connection strings from `PG_HOST`/`PG_PORT` + per-app `SqlConfig` (database_name, username, password). Maintain a pool cache keyed by `(database_name, username)`, lazily initialized on first use.
- [ ] Update `HostState` — add `sql_config` field populated from `ApplicationConfig` at invocation time.
- [ ] Wire `sql` into `Linker` — call `sql::add_to_linker` in `RuntimeState::new()`, implement the generated `sql::Host` trait on `HostState`.

### Phase 7b: Messaging Host Function

- [ ] Implement `messaging` interface in new `src/host_messaging.rs` — `send` publishes to NATS via the existing `async_nats::Client`.
- [ ] Update `HostState` — add `nats_client` field populated from `ApplicationConfig` at invocation time.
- [ ] Wire `messaging` into `Linker` — call `messaging::add_to_linker` in `RuntimeState::new()`, implement the generated `messaging::Host` trait on `HostState`.

### Phase 8: Sandboxing & Resource Limits

- [ ] Fuel metering — enable on `Engine`, set budget per `Store` before `on-message` (configurable via `WASM_FUEL_LIMIT`).
- [ ] Memory limits — configure `InstanceLimits` on `Engine` (e.g. 64 MB default, `WASM_MEMORY_LIMIT_MB`).
- [ ] Wall-clock timeout — wrap `spawn_blocking` in `tokio::time::timeout` (e.g. 30s default, `WASM_TIMEOUT_SECS`).
- [ ] Instance pooling (stretch goal) — `PoolingAllocationConfig` for pre-allocated slots; defer if per-invocation model performs adequately.

### Phase 9: README Alignment

- [ ] Replace wildcard `fn.>` / `NATS_TOPIC_PREFIX` description with per-topic subscription model and internal prefix scheme.
- [ ] Replace `for_each_concurrent` reference with actual concurrency description.
- [ ] Verify module loading section matches Phase 1 implementation.
- [ ] Document gateway in execution-host README (NATS reply flow, platform JSON payload format, two-world dispatch).
- [ ] Document the two WIT worlds in the project README and `framework/` — note `message-application` for pure message-passing and `http-application` for HTTP endpoints.
- [ ] Full pass for any remaining stale claims.

### Verification

After every phase, run `tilt ci` to confirm the full stack deploys and the end-to-end test passes. A phase is not complete until `tilt ci` exits 0.
