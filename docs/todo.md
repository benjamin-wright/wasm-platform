# Plan: Config Sync Between wp-operator and execution-host

## TL;DR

Wire the gRPC ConfigSync service defined in `configsync.proto` end-to-end: implement the server in the Go operator, implement the client in the Rust execution host, add Helm plumbing for service discovery, and fill in the operator's `Reconcile()` loop to drive config pushes. Real PostgreSQL, Redis, and NATS instances are running via the `wp-databases` chart — the reconciler should wire real connection details rather than stubs.

---

## Phase 1 — Operator gRPC Server

Stand up the ConfigSync gRPC server inside the wp-operator process, backed by an in-memory config store that the reconciler will write to.

### Steps

1. **Create an in-memory config store** (`internal/configstore/store.go`)
   - Thread-safe map: `map[types.NamespacedName]*configsync.ApplicationConfig`
   - Methods: `Set(key, config)`, `Delete(key)`, `Snapshot() []*configsync.ApplicationConfig` (returns a copy), `Version() uint64` (monotonically increasing counter bumped on every mutation)
   - The store also maintains a registry of connected host streams (see step 3)

2. **Implement `ConfigSyncServer`** (`internal/grpc/server.go`)
   - Embed `configsync.UnimplementedConfigSyncServer`
   - Accept a `*configstore.Store` in the constructor
   - `RequestFullConfig`: read `store.Snapshot()` and `store.Version()`, construct `FullConfigResponse` with `FullConfig`, return it
   - `PushIncrementalUpdate`: register the stream in the store's host registry keyed by `host_id` (received from the first ack or a metadata header). Block on a per-host channel; when the reconciler enqueues an `IncrementalUpdateRequest`, send it on the stream and wait for the `IncrementalUpdateAck`. Handle stream closure gracefully (deregister host)

3. **Add a broadcast helper to the store** (`internal/configstore/store.go`)
   - `BroadcastUpdate(update *configsync.IncrementalConfig)` — fans the update out to every registered host channel
   - Each host channel is buffered (small buffer, e.g. 16); if a host falls behind, log a warning and let it reconnect via `RequestFullConfig`

4. **Start the gRPC listener in `cmd/main.go`**
   - After creating the controller-runtime manager, create the `configstore.Store`
   - Launch `grpc.NewServer()` on a configurable port (env `GRPC_PORT`, default `50051`) in a goroutine
   - Register `ConfigSyncServer` with the gRPC server
   - Wire graceful shutdown: use manager's context cancellation to call `grpcServer.GracefulStop()`
   - Pass the store into the `ApplicationReconciler` (see Phase 2)

### Files to create/modify
- **Create** `components/wp-operator/internal/configstore/store.go`
- **Create** `components/wp-operator/internal/grpc/server.go`
- **Modify** `components/wp-operator/cmd/main.go` — add gRPC listener, store init, and dependency injection

---

## Phase 2 — Operator Reconcile Loop

Fill in the `ApplicationReconciler.Reconcile()` method so that each CRD change updates the config store and triggers a push.

### Steps

5. **Handle deletion** (*depends on step 1*)
   - Check for `DeletionTimestamp`; if set, call `store.Delete(namespacedName)`, build an `AppUpdate{config, delete: true}`, call `store.BroadcastUpdate(...)`, remove finalizer, return
   - Add a finalizer on create/update to ensure deletions are observed

6. **Handle create/update** (*depends on step 1*)
   - Read the `Application` CR
   - **Stub**: resolve OCI digest — for now, copy `spec.module` directly into `resolvedImage` status field and `module_ref` proto field. Leave a `// TODO: resolve mutable tags via OCI registry` comment
   - **Real**: database provisioning — if `spec.sql` is set, look up the PostgreSQL connection URL from the provisioned db-operator `Database` CR (or a well-known Secret produced by the db-operator) and populate `SqlConfig.connection_url` with the real value
   - **Real**: key-value provisioning — look up the Redis connection URL from the provisioned db-operator `Redis` CR / Secret and populate `KeyValueConfig.connection_url`
   - **Real**: NATS consumer creation — use the NATS connection details from the `wp-databases` chart (service `nats:4222` by default) to create the consumer and populate `NatsConfig`
   - Build `ApplicationConfig` proto message from spec + stubs
   - Call `store.Set(namespacedName, appConfig)`, then `store.BroadcastUpdate(...)` with `delete: false`
   - Update status conditions (`Ready` → reason `ConfigPushed` or `ProvisioningStubbed`)

7. **Write integration tests** (*parallel with steps 5–6*)
   - Use envtest (controller-runtime's test harness) to verify:
     - Creating an `Application` CR adds it to the store
     - Updating an `Application` CR replaces it in the store
     - Deleting an `Application` CR removes it and broadcasts a delete update

### Files to modify
- **Modify** `components/wp-operator/internal/controller/application_controller.go` — implement `Reconcile()`, inject `Store`
- **Create** `components/wp-operator/internal/controller/application_controller_test.go` — envtest integration tests

---

## Phase 3 — Execution-Host gRPC Client

Add a gRPC client to the Rust execution host that connects to the operator, requests a full config snapshot on startup, then maintains the incremental update stream.

### Steps

8. **Import generated stubs and define config state** (*no dependencies*)
   - In a new `src/config.rs` module, include the tonic-generated module via `tonic::include_proto!("configsync.v1")`
   - Define an `AppRegistry` struct: `Arc<RwLock<HashMap<(String, String), ApplicationConfig>>>` (keyed by `(namespace, name)`)
   - Implement `apply_full_config(&self, full: FullConfig)` (replaces entire map) and `apply_incremental(&self, updates: Vec<AppUpdate>)` (upserts/deletes)

9. **Implement startup full-config request** (*depends on step 8*)
   - Read operator address from env `CONFIG_SYNC_ADDR` (e.g. `http://wp-operator:50051`)
   - Create `ConfigSyncClient::connect(addr)` via tonic
   - Call `request_full_config(FullConfigRequest { host_id, last_ack_timestamp: None })`
   - Apply the response to `AppRegistry`
   - `host_id`: derive from `HOSTNAME` env var (pod name in k8s)

10. **Implement incremental update stream** (*depends on step 9*)
    - Call `push_incremental_update()` to open the bidirectional stream
    - Spawn a tokio task that:
      - Receives `IncrementalUpdateRequest` from the stream
      - Calls `registry.apply_incremental(updates)`
      - Sends back `IncrementalUpdateAck { host_id, version_applied, success: true, message: "" }`
    - On stream error/close, log a warning and loop back to step 9 (reconnect with full config request) after a backoff delay

11. **Integrate with existing NATS subscription** (*depends on step 8*)
    - Current code subscribes to a single hardcoded topic prefix. Modify this to be driven by the `AppRegistry`:
      - On startup after full config is applied: subscribe to each app's `topic`
      - For now, all apps share the single preloaded module (existing POC behaviour). Per-app module loading is out of scope.
      - When incremental updates arrive with new topics, subscribe; when apps are deleted, unsubscribe
    - This is a minimal integration — the existing `invoke_on_message()` function is reused

### Files to create/modify
- **Create** `components/execution-host/src/config.rs` — `AppRegistry`, config application logic
- **Modify** `components/execution-host/src/main.rs` — gRPC client startup, reconnect loop, NATS subscription changes

---

## Phase 4 — Helm & Service Discovery

Wire Kubernetes networking so execution-host pods can reach the operator's gRPC server.

### Steps

12. **Expose gRPC port on the operator** (*no code dependency, parallel with Phases 1–3*)
    - Add `grpc.port: 50051` to `components/wp-operator/helm/values.yaml`
    - Add container port `50051` to the operator Deployment template
    - Create a new Service (or add a port to the existing Service) named `wp-operator-grpc` exposing port `50051`

13. **Inject operator address into execution-host** (*depends on step 12*)
    - Add `configSync.serverAddr` to `components/execution-host/helm/values.yaml`, defaulting to `http://wp-operator-grpc:50051`
    - Add env var `CONFIG_SYNC_ADDR` to the execution-host Deployment template, sourced from that value
    - Add env var `HOST_ID` sourced from `metadata.name` (pod name) via the downward API

### Files to modify
- **Modify** `components/wp-operator/helm/values.yaml`
- **Modify** `components/wp-operator/helm/templates/` — deployment and service templates
- **Modify** `components/execution-host/helm/values.yaml`
- **Modify** `components/execution-host/helm/templates/` — deployment template

---

## Relevant Files

| File | Role |
|------|------|
| `proto/configsync/v1/configsync.proto` | API contract — **read-only**, do not modify |
| `components/wp-operator/internal/grpc/configsync/` | Generated Go gRPC stubs — **read-only**, regenerate via `make generate-proto` |
| `components/wp-operator/internal/controller/application_controller.go` | Reconcile loop — currently a stub |
| `components/wp-operator/cmd/main.go` | Operator entrypoint — add gRPC server startup |
| `components/wp-operator/api/v1alpha1/application_types.go` | CRD types — reference for mapping spec → proto |
| `components/execution-host/src/main.rs` | Host entrypoint — add gRPC client, modify NATS subscription logic |
| `components/execution-host/build.rs` | Tonic codegen — already configured, no changes needed |
| `components/execution-host/Cargo.toml` | Rust deps — tonic/prost already present |
| `framework/runtime.wit` | WIT interface — reference only, no changes |

---

## Verification

1. **Unit/integration tests (Phase 2, step 7)**: `cd components/wp-operator && make test` — envtest verifies CRD → store → broadcast pipeline
2. **Operator gRPC smoke test**: Run the operator locally, use `grpcurl` to call `configsync.v1.ConfigSync/RequestFullConfig` and verify an empty (or populated) response
3. **Execution-host connection test**: Start operator and host locally (or via Tilt); verify host logs show successful `RequestFullConfig` call and stream establishment
4. **End-to-end via Tilt**: `tilt up`, create an `Application` CR via `kubectl apply`, observe:
   - Operator logs: reconcile fires, config stored, broadcast sent
   - Host logs: incremental update received, ack sent, NATS subscription created for the app's topic
5. **Delete flow**: `kubectl delete application <name>`, verify host logs show app removal and NATS unsubscribe

---

## Decisions

- **Proto is frozen** for this plan — no changes to `configsync.proto`
- **Data-layer provisioning is real** — PostgreSQL, Redis, and NATS are live via the `wp-databases` chart; the reconciler reads connection details from the db-operator-produced Secrets / CRs and passes real URLs into the config proto
- **Per-app module loading is out of scope** — all apps use the single preloaded module for now; this is tracked separately
- **Host-side reconnection** uses simple retry-with-backoff → full config re-request; no partial resync logic needed yet
- **No TLS for gRPC** in-cluster — operators and hosts communicate within the cluster network; mTLS can be added later via a service mesh if needed
- **Scope includes** minimal NATS subscription management (subscribe/unsubscribe per app topic) since it's tightly coupled to config application

## Further Considerations

1. **Host registration model**: The current proto uses the bidirectional stream for operator→host pushes, where the host's identity comes from ACKs or metadata. An alternative is server-sent streaming (operator→host only) with a simpler model. **Recommendation**: stick with the existing bidirectional proto design since stubs are already generated.
2. **Concurrency in the store**: The broadcast fan-out blocks until all host channels accept. If a host is slow, it could back-pressure the reconciler. **Recommendation**: use buffered channels with overflow detection; if a host's buffer is full, close its stream and let it reconnect.
3. **Leader election and gRPC**: Only the leader operator replica should serve gRPC to avoid split-brain config pushes. **Recommendation**: gate the gRPC server on leader election (controller-runtime already supports this); non-leader replicas don't start the gRPC listener.

---

# ~~Plan: Factor DB CRDs into wp-databases chart~~ ✅ COMPLETE

## TL;DR

~~Extract the three db-operator CRD templates (`postgres.yaml`, `redis.yaml`, `nats.yaml`) from the `wasm-platform` umbrella chart into a new `wp-databases` Helm chart at `components/wp-databases/` (no extra `helm/` nesting — no source code). Wire it back as a file dependency of `wasm-platform`. Create `components/wp-databases/Tiltfile` (co-located with the chart, matching other component conventions) exporting `db_operator(namespace)` and `wp_databases(namespace)` as separate functions.~~

The `wp-databases` chart is implemented and the databases (PostgreSQL, Redis, NATS) are deployed and running.

---

## Phase 1 — Create wp-databases chart

1. Create `components/wp-databases/Chart.yaml` — Application chart, name `wp-databases`, version 0.1.0
2. Create `components/wp-databases/values.yaml` — postgres, redis, nats defaults
3. Create `components/wp-databases/templates/_helpers.tpl` — define `wp-databases.labels`
4. Create `components/wp-databases/templates/postgres.yaml`, `redis.yaml`, `nats.yaml` — moved from wasm-platform, label helper updated to `wp-databases.labels`

## Phase 2 — Update wasm-platform chart

5. Add `wp-databases` file dependency to `helm/wasm-platform/Chart.yaml`
6. Nest postgres/redis/nats values under `wp-databases:` key in `helm/wasm-platform/values.yaml`
7. Delete `helm/wasm-platform/templates/postgres.yaml`, `redis.yaml`, `nats.yaml`

## Phase 3 — Tiltfile

8. Create `components/wp-databases/Tiltfile` — exports `db_operator(namespace)` and `wp_databases(namespace)`
9. Update root `Tiltfile` — remove `## Install DB Operator ##` block, load and call both functions

## Decisions

- Chart and Tiltfile both at `components/wp-databases/` — no `helm/` subdirectory; consistent with other components
- `db_operator()` creates its own namespace; `wp_databases()` does not
- `wasm-platform/values.yaml` uses `wp-databases:` subchart key

## Further Considerations

1. **`helm dependency update`**: Must be run manually (`helm dependency update helm/wasm-platform`) after this change before Helm or Tilt will resolve the subchart.
2. **README.md**: Project convention (`components/*/README.md`) — not in scope for this task.
