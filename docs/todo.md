# TODO

Active implementation plan for the wasm-platform project.

---

## Completed Work

### NATS Credential Resilience ✅

Replaced hard-crash-on-NATS-failure with a resilient connection manager (`run_nats_manager`) that reads credentials from files at `NATS_CREDENTIALS_PATH` (Kubernetes secret volume mount), re-reads on every connection attempt, and reconnects on `AuthorizationViolation`. Added `/readyz` endpoint (503 until both NATS and config sync are ready). The four `NATS_*` env vars were replaced by `NATS_CREDENTIALS_PATH`.

### Phase 1: Per-Application Module Management ✅

`ModuleRegistry` maps `(namespace, name)` → loaded `Component`. On config change, checks the module cache, falls back to OCI pull + AOT compile, pushes `.cwasm` back to cache. Messages are routed by NATS subject via `AppRegistry::get_by_topic()`.

### Phase 2: Topic Uniqueness Enforcement ✅

Cluster-wide `spec.topic` uniqueness enforced by the wp-operator. Oldest `creationTimestamp` wins (tiebreak: lexicographic `namespace/name`). Blocked apps get `Ready: False / TopicConflict`. Self-healing: re-evaluates blocked apps when the owner is deleted or changes topic. Wildcards (`*`, `>`) banned via CRD validation.

### Phase 3: WIT Interface Split ✅

Split `framework/runtime.wit` into `world message-application` (`on-message`) and `world http-application` (`on-request` with typed `http-request`/`http-response` records). Execution host dispatches on `ApplicationConfig.world_type`. Custom WIT records used instead of WASI HTTP resource model.

### Phase 4: Internal Topic Prefixing & HTTP CRD ✅

Topic-only apps prefixed with `fn.`, HTTP apps auto-generate `http.<namespace>.<name>`. Added `spec.http` field (path + methods), mutually exclusive with `spec.topic` via CEL validation. The execution host subscribes to whatever the operator sends — no structural change needed.

### Phase 5: HTTP Gateway ✅

Rust gateway (`components/gateway/`) accepts HTTP traffic, serialises to platform-private JSON, publishes via NATS request-reply, deserialises the response. Route table synced from wp-operator via `GatewayRoutes` gRPC service. Method enforcement, `504` timeout handling.

### Phase 5b: hello-world Example ✅

Converted to `http-application` world. Packaged as OCI artifact via `oras push`. Wired into Tilt dev loop with build → push → apply chain and hot-reload via annotation patching.

---

## Active Work

### Phase 6a: SQL Host Function

Depends on Phase 1. Independent of 6b and 6c.

**Connection model:** the execution host is configured once with `PG_HOST`/`PG_PORT` env vars. Per-app credentials (database name, username, password) arrive via `SqlConfig` in the `ConfigSync` stream. Connection pools are per-application, keyed by `(database_name, username)`, lazily initialized, and shared across invocations.

- [ ] Implement `sql` interface in `src/host_sql.rs` — `query`/`execute` backed by `tokio-postgres`. Build connection strings from shared host/port + per-app `SqlConfig`. Pool cache keyed by `(database_name, username)`.
- [ ] Update `HostState` — add `sql_config` field populated from `ApplicationConfig` at invocation time.
- [ ] Wire `sql` into `Linker` — `sql::add_to_linker` in `RuntimeState::new()`, implement the generated `sql::Host` trait on `HostState`.

### Phase 6b: KV Host Function

Depends on Phase 1. Independent of 6a and 6c.

**Connection model:** single Redis multiplexed connection shared across all apps (`REDIS_URL` env var). `KeyValueConfig` carries only the `prefix` for per-app isolation.

- [ ] Implement `kv` interface in `src/host_kv.rs` — `get`/`set`/`delete` via `redis` crate, transparently prepending `KeyValueConfig.prefix` to all keys.
- [ ] Add `redis_client: redis::Client` to `RuntimeState` — created from `REDIS_URL` at startup.
- [ ] Update `HostState` — add `kv_prefix: String` and `redis_client: redis::Client`.
- [ ] Wire `kv` into `Linker`.

### Phase 6c: Messaging Host Function

Depends on Phase 1. Independent of 6a and 6b.

- [ ] Implement `messaging` interface in `src/host_messaging.rs` — `send` publishes via the existing `async_nats::Client`.
- [ ] Update `HostState` — add `nats_client`.
- [ ] Wire `messaging` into `Linker`.

### Phase 7: Sandboxing & Resource Limits

Independent of Phases 6a–6c.

- [ ] Fuel metering — enable on `Engine`, budget per `Store` (`WASM_FUEL_LIMIT`).
- [ ] Memory limits — `InstanceLimits` (default 64 MB, `WASM_MEMORY_LIMIT_MB`).
- [ ] Wall-clock timeout — `tokio::time::timeout` around `spawn_blocking` (`WASM_TIMEOUT_SECS`).
- [ ] Instance pooling (stretch) — `PoolingAllocationConfig` for pre-allocated slots.

### Phase 8: README Alignment

Depends on Phases 6–7.

- [ ] Update execution-host README to match final implementation (concurrency model, probe endpoints, module loading flow).
- [ ] Document the two WIT worlds in the project README.
- [ ] Full pass for stale claims across all READMEs.

### Outstanding Items

- [ ] End-to-end test — full stack: create `Application` with `spec.http`, HTTP request through gateway, verify module executes with host functions and response returns.
- [ ] Unit tests for `ModuleRegistry` (mock HTTP cache) and host functions (fake connections behind traits).
- [ ] `cargo clippy` + `cargo fmt` pass.
- [ ] Helm chart verification with updated config.
