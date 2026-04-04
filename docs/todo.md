# TODO

Active implementation plan for the wasm-platform project.

---

## Execution Host: Align Implementation with README Spec

The execution host currently loads a single hardcoded WASM module for all messages and implements none of the WIT host functions (`sql`, `kv`, `messaging`). The README specifies per-application module management via the module cache, host function implementations backed by PostgreSQL/Redis/NATS, and sandboxing. This plan closes those gaps in four phases, updating the README last to match the per-topic NATS model.

### Gap Summary

| Area | README Spec | Current Code |
|---|---|---|
| Module loading | Per-app, from module cache (digest/arch/version) | Single `WASM_MODULE_PATH` at startup |
| Message routing | Per-app by NATS subject → app config | All messages → single module |
| Host functions | `sql`, `kv`, `messaging` WIT imports | Only WASI linked |
| Data isolation | Redis key prefix, per-app SQL credentials | Not implemented |
| NATS model | Wildcard `fn.>` from `NATS_TOPIC_PREFIX` | Per-topic dynamic subs (correct) |
| Sandboxing | Fuel, memory limits, timeouts | None |

### Decisions

- **NATS:** per-topic subscriptions (current code) are correct; README will be updated.
- **Concurrency:** semaphore approach is kept (functionally equivalent to `for_each_concurrent`).
- **Instance pooling:** deferred as stretch goal.
- **PG/Redis connections:** pool-per-app, lazily initialized, shared across invocations — not per-invocation.
- **Out of scope:** Gateway, Trigger Layer, Token Service.

---

### Phase 1: Per-Application Module Management

- [x] Create `ModuleRegistry` in new `src/modules.rs` — maps `(namespace, name)` → loaded `Component`, behind `Arc<RwLock<...>>`.
- [x] Add module-cache HTTP client (`reqwest`) — on config change, check cache via `GET /modules/{digest}/{arch}/{version}`.
- [x] Add OCI pull + AOT compile on cache miss (`oci-distribution`) — pull raw `.wasm`, call `engine.precompile_component()`, push `.cwasm` back to module cache via `PUT`.
- [x] Wire config sync → module loading — when `AppRegistry` receives a new/updated app, trigger the cache-check → pull → compile → register flow.
- [x] Route messages by subject — in `process_nats_messages`, use `AppRegistry::get_by_topic()` to find the app, then `ModuleRegistry` to get the `Component`.
- [x] Parameterize `invoke_on_message` — accept the per-app `Component` rather than using a single shared one.

### Phase 2: Topic Uniqueness Enforcement (wp-operator)

Independent of Phases 1, 3, and 4. Must be completed before any Application reaches production use.

#### Design decisions

- **Scope:** `spec.topic` must be unique cluster-wide (across all namespaces). NATS subscriptions are global; two apps in different namespaces claiming the same subject would silently compete for messages.
- **Winner:** the Application with the oldest `creationTimestamp` owns the topic. Tiebreak (same timestamp, e.g. batch-created): lexicographic sort on `namespace/name`, lower sorts first.
- **Claim point:** the claim is based on `spec.topic` alone — no need to have successfully reconciled first. This prevents a window where two apps both proceed into side-effect territory.
- **Blocked app behaviour:** reconcile short-circuits immediately after conflict detection. No NATS consumer, no SQL provisioning, no config pushed to execution hosts. `Ready: False`, reason `TopicConflict`, message names the owning app (e.g. `topic "foo.messages" is already claimed by default/other-app`).
- **Healing on owner change:** the watch handler is surgical — it only wakes apps waiting on the *freed* topic, not all blocked apps. A `spec.topic` field index on the cache makes this efficient. On delete, enqueue all apps sharing the deleted app's topic. On update where the topic changed, enqueue all apps sharing the *old* topic (available via `TypedUpdateEvent.ObjectOld`). On create, no wake-up is needed — a new app can only block others, never free a claim.
- **Wildcards banned:** `spec.topic` must not contain `*` or `>`. Enforced via a CRD validation marker (no webhook needed). README and wp-operator README both updated to document this constraint.

#### Tasks

- [ ] Add `+kubebuilder:validation:Pattern` marker to `spec.topic` in `application_types.go` — reject any value containing `*` or `>` (pattern: `^[^*>]+$`). Regenerate CRD manifest (`make generate`).
- [ ] Add cluster-scoped RBAC marker for listing Applications — `+kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=list;watch` with no namespace qualifier, and update the operator's `ClusterRole` in the Helm chart accordingly.
- [ ] Register a `spec.topic` field index on the Application cache in `SetupWithManager` — used by both `findTopicOwner` and the watch handler to avoid full-list scans.
- [ ] Implement `findTopicOwner(ctx, client, topic, self) (*Application, error)` helper — queries the cache via the `spec.topic` index, excludes self, returns the app with the oldest `creationTimestamp` (tiebreak: `namespace/name` lexicographic order). Returns `nil` if the calling app is the rightful owner.
- [ ] Add conflict short-circuit to `reconcileUpsert` — call `findTopicOwner` before any side-effectful work; if an owner is found, set `Ready: False / TopicConflict` with the owner named in the message, update status, and return without requeueing.
- [ ] Add secondary `Watches` in `SetupWithManager` using `TypedUpdateEvent` — on delete, use the topic index to enqueue all apps sharing the deleted app's `spec.topic`; on update where `spec.topic` changed, enqueue all apps sharing `ObjectOld.Spec.Topic`; skip on create.
- [ ] Clear `TopicConflict` condition on successful reconcile — ensure `setReadyCondition(True, ...)` removes or supersedes any stale `TopicConflict` condition.
- [ ] Update `components/wp-operator/README.md` — document the wildcard ban, the cluster-wide uniqueness rule, the `TopicConflict` condition, and the self-healing behaviour.
- [ ] Tests — table-driven unit tests for `findTopicOwner` covering: sole owner, two apps same topic (older wins), two apps same topic same timestamp (name tiebreak), three-way race; integration test for the blocked→unblocked healing flow.

---

### Phase 3: Host Function Implementations

Depends on Phase 1 (per-app routing provides the `ApplicationConfig` at invocation time).

- [ ] Implement `sql` interface in new `src/host_sql.rs` — `query`/`execute` backed by `tokio-postgres` using per-app `SqlConfig.connection_url`. Connection pools are per-application, lazily initialized, shared across invocations.
- [ ] Implement `kv` interface in new `src/host_kv.rs` — `get`/`set`/`delete` backed by `redis` crate using `KeyValueConfig.connection_url`, transparently prepending `KeyValueConfig.prefix` to all keys.
- [ ] Implement `messaging` interface in new `src/host_messaging.rs` — `send` publishes to NATS via the existing `async_nats::Client`.
- [ ] Wire into `Linker` — call `add_to_linker` for each WIT interface in `RuntimeState::new()`, implement generated `Host` traits on `HostState`.
- [ ] Update `HostState` — add per-invocation fields (`sql_config`, `kv_config`, `nats_client`) populated from the `ApplicationConfig` at invocation time.

### Phase 4: Sandboxing & Resource Limits

Parallel with Phases 1–3 (no dependency on per-app routing).

- [ ] Fuel metering — enable on `Engine`, set budget per `Store` before `on-message` (configurable via `WASM_FUEL_LIMIT`).
- [ ] Memory limits — configure `InstanceLimits` on `Engine` (e.g. 64 MB default, `WASM_MEMORY_LIMIT_MB`).
- [ ] Wall-clock timeout — wrap `spawn_blocking` in `tokio::time::timeout` (e.g. 30s default, `WASM_TIMEOUT_SECS`).
- [ ] Instance pooling (stretch goal) — `PoolingAllocationConfig` for pre-allocated slots; defer if per-invocation model performs adequately.

### Phase 5: README Alignment

Depends on Phases 1–4.

- [ ] Replace wildcard `fn.>` / `NATS_TOPIC_PREFIX` description with per-topic subscription model.
- [ ] Replace `for_each_concurrent` reference with actual concurrency description.
- [ ] Verify module loading section matches Phase 1 implementation.
- [ ] Full pass for any remaining stale claims.

### Verification

- [ ] Unit tests for `ModuleRegistry` — mock HTTP server for cache round-trip.
- [ ] Unit tests for host functions — fake external connections behind traits.
- [ ] Integration test — full stack in test namespace: create `Application` CR, send NATS message, verify guest executes with host functions.
- [ ] `cargo clippy` + `cargo fmt` pass.
- [ ] Helm chart deploys correctly with updated config.
