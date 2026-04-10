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

---

### Phase 7a: SQL Host Function

#### Design

The execution host is configured once with the shared PostgreSQL host and port (`PG_HOST`, `PG_PORT` env vars). Per-app credentials (database name, username, password) arrive via `SqlConfig` in the `ConfigSync` stream. The host builds connection strings internally by combining the shared host/port with the per-app credentials. Connection pools are per-application, keyed by `(database_name, username)`, lazily initialized, and shared across invocations.

#### Tasks

- [ ] Implement `sql` interface in new `src/host_sql.rs` — `query`/`execute` backed by `tokio-postgres`. Build connection strings from `PG_HOST`/`PG_PORT` + per-app `SqlConfig` (database_name, username, password). Maintain a pool cache keyed by `(database_name, username)`, lazily initialized on first use.
- [ ] Update `HostState` — add `sql_config` field populated from `ApplicationConfig` at invocation time.
- [ ] Wire `sql` into `Linker` — call `sql::add_to_linker` in `RuntimeState::new()`, implement the generated `sql::Host` trait on `HostState`.

### Phase 7b: Messaging Host Function

#### Design

The `messaging::send` WIT function publishes a raw byte payload to a user-supplied topic via the existing `async_nats::Client` on `HostState`. The topic is passed through the execution host's internal prefix scheme (`fn.<topic>`) so cross-module messaging stays within the platform namespace.

**E2E fixture — two cooperating modules:**
- **hello-world** (HTTP trigger, existing): on each request it publishes a message to topic `hello-world.events` (raw bytes, e.g. `b"tick"`), increments its own `requests` KV counter, and reads both the `requests` and `messages` KV keys. The response body becomes `requests=N messages=M`.
- **message-counter** (new `message-application` example): subscribes to topic `hello-world.events`. On every received message it increments `messages` in the shared `hello-world` KV store. No HTTP trigger.

The e2e test hits `GET /hello` twice and asserts that both `requests=N` and `messages=M` increase between calls, proving the full publish → subscribe → KV-write path is exercised.

#### Tasks

- [x] Implement `messaging` interface in new `src/host_messaging.rs` — `send` publishes to NATS via the existing `async_nats::Client` using the `fn.<topic>` prefix scheme.
- [x] Update `HostState` — add `nats_client` field populated from `ApplicationConfig` at invocation time.
- [x] Wire `messaging` into `Linker` — call `messaging::add_to_linker` in `RuntimeState::new()`, implement the generated `messaging::Host` trait on `HostState`.
- [x] Update hello-world module — call `messaging::send("hello-world.events", b"tick")` on each request; read both `requests` and `messages` KV keys; update response body to `requests=N messages=M`.
- [x] Add `examples/message-counter/` — `Cargo.toml`, `src/lib.rs` implementing `world message-application`; on each `on-message` call increment `messages` in the `hello-world` KV store.
- [x] Add `examples/message-counter/k8s/application.yaml` — `spec.topic: hello-world.events`, `spec.keyValue: hello-world` (shares the store with hello-world).
- [x] Add `examples/message-counter/Tiltfile` — build and deploy the module; add it as a dep of `e2e-tests`.
- [x] Load `examples/message-counter/Tiltfile` from root `Tiltfile`.
- [x] Update `tests/e2e/e2e_test.go` — parse `messages=M` from the response body in addition to `requests=N`; assert both counters increment across two calls.

### Phase 8: Sandboxing & Resource Limits

#### Tasks

- [ ] Fuel metering — enable on `Engine`, set budget per `Store` before `on-message` (configurable via `WASM_FUEL_LIMIT`).
- [ ] Memory limits — configure `InstanceLimits` on `Engine` (e.g. 64 MB default, `WASM_MEMORY_LIMIT_MB`).
- [ ] Wall-clock timeout — wrap `spawn_blocking` in `tokio::time::timeout` (e.g. 30s default, `WASM_TIMEOUT_SECS`).
- [ ] Instance pooling (stretch goal) — `PoolingAllocationConfig` for pre-allocated slots; defer if per-invocation model performs adequately.

### Phase 9: README Alignment

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
