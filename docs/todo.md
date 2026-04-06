# TODO

Active implementation plan for the wasm-platform project.

---

## NATS Credential Resilience ✅

Resolved a crash-loop / permanent misconfiguration caused by the execution host treating NATS as a hard startup requirement. Previously `nats::connect()` in `main()` fatally crashed the process on any error; Kubernetes back-off then made recovery take minutes. Credentials were injected as env vars (`envFrom: secretRef`) and were frozen at pod start — credential rotation would never be picked up without a pod restart.

**Changes made:**
- `src/nats.rs` — replaced `connect()` with `run_nats_manager(credentials_path, client_tx, ready_tx)`. Reads credentials from files at `NATS_CREDENTIALS_PATH` (Kubernetes secret volume mount); re-reads on every connection attempt. Registers an `async_nats` event callback to detect `AuthorizationViolation` and trigger immediate re-read + reconnect.
- `src/nats.rs` — `manage_nats_subscriptions` now accepts `watch::Receiver<Option<Client>>`; drops and re-subscribes all topics on client replacement; drops subscriptions when client is `None`.
- `src/config_sync.rs` — `run_config_sync_loop` accepts a `synced_tx: watch::Sender<bool>`; sets `true` after the first successful full snapshot, `false` on error.
- `src/main.rs` — process no longer crashes on NATS unavailability. Added `/readyz` endpoint (503 until both NATS and config sync are ready). `/healthz` remains the liveness probe (always 200). `NATS_CREDENTIALS_PATH` is now required instead of the four `NATS_*` env vars.
- `helm/templates/deployment.yaml` — replaced `envFrom: secretRef` with a projected secret volume at `/var/run/secrets/nats`; added `NATS_CREDENTIALS_PATH` env var; liveness → `/healthz`, readiness → `/readyz`.

---

## Execution Host: Align Implementation with README Spec

The execution host currently loads a single hardcoded WASM module for all messages and implements none of the WIT host functions (`sql`, `kv`, `messaging`). The README specifies per-application module management via the module cache, host function implementations backed by PostgreSQL/Redis/NATS, and sandboxing. This plan closes those gaps, adding an HTTP gateway and internal NATS topic prefixing along the way.

### Gap Summary

| Area | README Spec | Current Code |
|---|---|---|
| Module loading | Per-app, from module cache (digest/arch/version) | Single `WASM_MODULE_PATH` at startup |
| Message routing | Per-app by NATS subject → app config | All messages → single module |
| Host functions | `sql`, `kv`, `messaging` WIT imports | Only WASI linked |
| Data isolation | Redis key prefix, per-app SQL credentials | Not implemented |
| NATS model | Wildcard `fn.>` from `NATS_TOPIC_PREFIX` | Per-topic dynamic subs (correct) |
| Sandboxing | Fuel, memory limits, timeouts | None |
| HTTP ingress | Gateway translates HTTP → NATS → module → HTTP response | Not implemented |
| Topic prefixing | `fn.` / `http.` prefix by trigger type | Raw user-supplied topic |

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

### Phase 1: Per-Application Module Management ✅

- [x] Create `ModuleRegistry` in new `src/modules.rs` — maps `(namespace, name)` → loaded `Component`, behind `Arc<RwLock<...>>`.
- [x] Add module-cache HTTP client (`reqwest`) — on config change, check cache via `GET /modules/{digest}/{arch}/{version}`.
- [x] Add OCI pull + AOT compile on cache miss (`oci-distribution`) — pull raw `.wasm`, call `engine.precompile_component()`, push `.cwasm` back to module cache via `PUT`.
- [x] Wire config sync → module loading — when `AppRegistry` receives a new/updated app, trigger the cache-check → pull → compile → register flow.
- [x] Route messages by subject — in `process_nats_messages`, use `AppRegistry::get_by_topic()` to find the app, then `ModuleRegistry` to get the `Component`.
- [x] Parameterize `invoke_on_message` — accept the per-app `Component` rather than using a single shared one.

### Phase 2: Topic Uniqueness Enforcement (wp-operator) ✅

Independent of Phases 1, 3, and 4. Must be completed before any Application reaches production use.

#### Design decisions

- **Scope:** `spec.topic` must be unique cluster-wide (across all namespaces). NATS subscriptions are global; two apps in different namespaces claiming the same subject would silently compete for messages.
- **Winner:** the Application with the oldest `creationTimestamp` owns the topic. Tiebreak (same timestamp, e.g. batch-created): lexicographic sort on `namespace/name`, lower sorts first.
- **Claim point:** the claim is based on `spec.topic` alone — no need to have successfully reconciled first. This prevents a window where two apps both proceed into side-effect territory.
- **Blocked app behaviour:** reconcile short-circuits immediately after conflict detection. No NATS consumer, no SQL provisioning, no config pushed to execution hosts. `Ready: False`, reason `TopicConflict`, message names the owning app (e.g. `topic "foo.messages" is already claimed by default/other-app`).
- **Healing on owner change:** the watch handler is surgical — it only wakes apps waiting on the *freed* topic, not all blocked apps. A `spec.topic` field index on the cache makes this efficient. On delete, enqueue all apps sharing the deleted app's topic. On update where the topic changed, enqueue all apps sharing the *old* topic (available via `TypedUpdateEvent.ObjectOld`). On create, no wake-up is needed — a new app can only block others, never free a claim.
- **Wildcards banned:** `spec.topic` must not contain `*` or `>`. Enforced via a CRD validation marker (no webhook needed). README and wp-operator README both updated to document this constraint.

#### Tasks

- [x] Add `+kubebuilder:validation:Pattern` marker to `spec.topic` in `application_types.go` — reject any value containing `*` or `>` (pattern: `^[^*>]+$`). Regenerate CRD manifest (`make generate`).
- [x] Add cluster-scoped RBAC marker for listing Applications — `+kubebuilder:rbac:groups=wasm-platform.io,resources=applications,verbs=list;watch` with no namespace qualifier, and update the operator's `ClusterRole` in the Helm chart accordingly.
- [x] Register a `spec.topic` field index on the Application cache in `SetupWithManager` — used by both `findTopicOwner` and the watch handler to avoid full-list scans.
- [x] Implement `findTopicOwner(ctx, client, topic, self) (*Application, error)` helper — queries the cache via the `spec.topic` index, excludes self, returns the app with the oldest `creationTimestamp` (tiebreak: `namespace/name` lexicographic order). Returns `nil` if the calling app is the rightful owner.
- [x] Add conflict short-circuit to `reconcileUpsert` — call `findTopicOwner` before any side-effectful work; if an owner is found, set `Ready: False / TopicConflict` with the owner named in the message, update status, and return without requeueing.
- [x] Add secondary `Watches` in `SetupWithManager` using `TypedUpdateEvent` — on delete, use the topic index to enqueue all apps sharing the deleted app's `spec.topic`; on update where `spec.topic` changed, enqueue all apps sharing `ObjectOld.Spec.Topic`; skip on create.
- [x] Clear `TopicConflict` condition on successful reconcile — ensure `setReadyCondition(True, ...)` removes or supersedes any stale `TopicConflict` condition.
- [x] Update `components/wp-operator/README.md` — document the wildcard ban, the cluster-wide uniqueness rule, the `TopicConflict` condition, and the self-healing behaviour.
- [x] Tests — table-driven unit tests for `findTopicOwner` covering: sole owner, two apps same topic (older wins), two apps same topic same timestamp (name tiebreak), three-way race; integration test for the blocked→unblocked healing flow.

---

### Phase 3: WIT Interface Split ✅

Splits `framework/runtime.wit` into two worlds. **This is a breaking change for all existing guest modules and the execution host.** The `examples/hello-world` module must be updated alongside this phase.

#### New WIT shape

```wit
world message-application {
    import sql;
    import kv;
    import messaging;
    export on-message: func(payload: list<u8>) -> result<option<list<u8>>, string>;
}

record http-request {
    method: string,
    path: string,
    query: string,
    headers: list<tuple<string, string>>,
    body: option<list<u8>>,
}

record http-response {
    status: u16,
    headers: list<tuple<string, string>>,
    body: option<list<u8>>,
}

world http-application {
    import sql;
    import kv;
    import messaging;
    export on-request: func(request: http-request) -> result<http-response, string>;
}
```

#### Tasks

- [x] Update `framework/runtime.wit` — rename `world application` to `world message-application`. Add `http-request` and `http-response` records. Add `world http-application` with `on-request` export. (Developer must run `cargo build` in each component to verify bindgen output after this change.)
- [x] Add `world_type` enum to `configsync.proto` — values: `WORLD_TYPE_MESSAGE` (0), `WORLD_TYPE_HTTP` (1). Add `world_type` field to `ApplicationConfig`. Regenerate Go and Rust gRPC stubs (`make generate` in `components/wp-operator/`; `cargo build` triggers `build.rs` in `components/execution-host/`).
- [x] Update execution host `runtime.rs` — add second `bindgen!({ world: "http-application", ... })` block. Add `invoke_on_request(state, component, request: HttpRequest) -> Result<HttpResponse>` function alongside the existing `invoke_on_message`.
- [x] Update execution host `process_nats_messages` — dispatch on `ApplicationConfig.world_type`: `WORLD_TYPE_MESSAGE` topics use the existing `invoke_on_message` path; `WORLD_TYPE_HTTP` topics decode the platform JSON payload into an `HttpRequest` struct, call `invoke_on_request`, and serialise the returned `HttpResponse` struct back to JSON bytes for the NATS reply.
- [x] Update `examples/hello-world` — change `world: "application"` to `world: "message-application"` in the `wit_bindgen::generate!` call. Trait name changes from `Guest` to whatever `wit-bindgen` generates for the renamed world.
- [x] Update wp-operator `reconcileUpsert` — set `cfg.WorldType` based on trigger class: `WORLD_TYPE_HTTP` when `spec.http` is set, `WORLD_TYPE_MESSAGE` otherwise.

---

### Phase 4: Internal Topic Prefixing & HTTP CRD Field

Introduces the `fn.`/`http.` internal topic prefix scheme and adds the `spec.http` field. This phase is prerequisite for the gateway (Phase 4) and changes the NATS subjects the execution host subscribes to. No WIT changes required — the execution host still receives a flat payload via `on-message`.

#### Design

- **Topic-only apps** (`spec.topic` set, `spec.http` absent): the operator prefixes the user-supplied topic with `fn.` before pushing it to execution hosts in `ApplicationConfig.topic`. The user writes `my-app.events`; the NATS subject is `fn.my-app.events`. The `spec.topic` field index and uniqueness check continue to operate on the unprefixed value.
- **HTTP apps** (`spec.http` set, `spec.topic` absent): the operator auto-generates the topic as `http.<namespace>.<name>` and pushes it in `ApplicationConfig.topic`. The user never sees or sets a topic. The uniqueness check is unnecessary — the topic is derived from the unique `(namespace, name)` pair.
- **Mutual exclusivity:** exactly one of `spec.topic` or `spec.http` must be set. Enforced via a CEL validation rule on the CRD (or a webhook, if CEL is unavailable on the target cluster version). The operator also validates in `reconcileUpsert` as a defence-in-depth check.

#### `spec.http` field shape

```yaml
spec:
  http:
    path: /api/orders
    methods:
      - GET
      - POST
```

| Field | Type | Required | Description |
|---|---|---|---|
| `spec.http.path` | string | yes | URL path the gateway exposes. Must start with `/`. Must be unique cluster-wide (same ownership rules as `spec.topic`). |
| `spec.http.methods` | []string | no | Allowed HTTP methods. Defaults to all methods if omitted. Used by the gateway for `405 Method Not Allowed` responses and `Allow` header on `HEAD`/`OPTIONS`. Valid values: `GET`, `HEAD`, `POST`, `PUT`, `DELETE`, `PATCH`, `OPTIONS`. |

#### Tasks

- [x] Add `HttpConfig` struct to `application_types.go` with `Path` (required, validated `^/`) and `Methods` (optional, validated enum). Make `spec.topic` optional. Add CEL rule enforcing exactly one of `spec.topic` / `spec.http` is set. Existing `^[^*>]+$` pattern on `spec.topic` is unchanged. Regenerate CRD manifest (`make generate`).
- [x] Add `HttpConfig` message to `configsync.proto` — fields: `path` (string), `methods` (repeated string). Add optional `http` field to `ApplicationConfig`. The `topic` field remains and always carries the fully-prefixed internal subject. (The `world_type` field is added in Phase 3.)
- [x] Update `reconcileUpsert` in the operator — compute the internal topic: if `spec.topic` is set, prefix with `fn.`; if `spec.http` is set, generate `http.<namespace>.<name>`. Set `cfg.Topic` to the prefixed value. Populate `cfg.Http` when `spec.http` is present.
- [x] Update topic uniqueness check — `findTopicOwner` must compare the *prefixed* topic, or (more simply) only compare within the same trigger class. Since HTTP topics are derived from `(namespace, name)` they are inherently unique; the check only needs to run for `spec.topic` apps. No functional change to the existing index, since the index stores unprefixed values and the comparison scope is already correct.
- [x] Update `AppRegistry` in the execution host — no structural change needed. The `topic` field in `ApplicationConfig` already carries the full subject string; the registry is keyed by it. The execution host subscribes to whatever the operator sends.
- [x] Update `buildDeleteUpdate` in the operator — ensure the prefixed topic is used in the delete config so the execution host correctly identifies the app to remove.
- [x] Update tests — add cases for `fn.`-prefixed topics, `http.`-derived topics, and mutual exclusivity validation.
- [x] Update `components/wp-operator/README.md` — document `spec.http`, the internal prefix scheme (noting it is invisible to users), and the mutual exclusivity rule.

---

### Phase 5: HTTP Gateway

New Rust service (`components/gateway/`). Accepts HTTP traffic, serialises the request into a platform-private NATS payload, publishes to the application's auto-generated `http.<namespace>.<name>` subject, waits for the NATS reply, and constructs an HTTP response from the reply. Depends on Phase 3 (WIT split adds `invoke_on_request` to the execution host) and Phase 4 (CRD adds `spec.http` and the operator generates `http.` topics).

#### Design

- **Route table:** the gateway maintains an in-memory route table populated by the wp-operator via a gRPC `GatewayRoutes` service (analogous to `ConfigSync`). Each entry maps `(path, methods)` → NATS subject. On startup the gateway requests the full route set; ongoing changes arrive as incremental deltas.
- **NATS payload format:** the gateway serialises the HTTP request as a platform-private JSON object matching the fields of the `http-request` WIT record (method, path, query, headers, body). The execution host decodes this, calls `invoke_on_request` with typed WIT records (Phase 3), and serialises the returned `http-response` record back to JSON for the NATS reply. The module never sees JSON — this encoding is entirely internal to the platform.
- **NATS request-reply:** the gateway uses `async_nats::Client::request()` which publishes with an auto-generated reply subject and waits for one response. The execution host already publishes to `message.reply` when present — no additional execution host changes needed beyond Phase 3.
- **Timeouts:** gateway-side timeout on the NATS request (e.g. 30s default, `GATEWAY_TIMEOUT_SECS`). Returns `504 Gateway Timeout` on expiry.
- **Method enforcement:** if the request method is not in the route's `methods` list (and the list is non-empty), return `405 Method Not Allowed` with an `Allow` header.
- **No TLS for MVP.** TLS termination will be added later (likely via a sidecar or Kubernetes Ingress).
- **No auth middleware for MVP.** The JSON payload includes a `headers` map so a future auth middleware layer can inject `x-user-id` (or similar) before the NATS publish.

#### gRPC route service

New proto `proto/gateway/v1/gateway.proto`:

```protobuf
service GatewayRoutes {
  rpc RequestFullRoutes(FullRoutesRequest) returns (FullRoutesResponse);
  rpc PushRouteUpdate(stream RouteUpdateAck) returns (stream RouteUpdateRequest);
}

message RouteConfig {
  string path = 1;
  repeated string methods = 2;
  string nats_subject = 3;
}
```

The wp-operator implements this service (same binary, new gRPC endpoint) and pushes route updates whenever an HTTP-type Application is created, updated, or deleted.

#### Tasks

- [x] Define `proto/gateway/v1/gateway.proto` with the `GatewayRoutes` service, request/response types, and `RouteConfig` message.
- [x] Implement `GatewayRoutes` server in the wp-operator — maintain a route store (`internal/routestore/store.go`, similar to `configstore.Store`), push incremental updates when HTTP-type Applications change. Registered on the same gRPC server instance as `ConfigSync`.
- [x] Scaffold `components/gateway/` — new Rust binary with `Cargo.toml`, `build.rs`, `Dockerfile`, Helm chart, `Tiltfile`, and `README.md`.
- [x] Implement route sync client in the gateway — gRPC client that connects to the wp-operator, requests full routes on startup, then maintains the incremental stream. Populates an in-memory `RouteTable` (path → `RouteEntry { methods, nats_subject }`).
- [x] Implement HTTP server in the gateway (`axum`) — on each request, look up the path in the `RouteTable`. If not found → `404`. If method not allowed → `405`. Otherwise serialise the request as a platform JSON payload (fields matching `http-request`), publish via `async_nats::Client::request()`, deserialise the `http-response` JSON from the reply, and return the HTTP response.
- [x] Add gateway to the platform `Tiltfile` and gateway Helm chart to `components/gateway/helm/`.
- [ ] End-to-end test — apply an Application with `spec.http`, verify the execution host pre-compiles the module, send an HTTP request to the gateway, confirm the request is routed through NATS to the module and the response is returned.
- [x] Update `components/gateway/README.md` — document the route sync protocol, platform JSON payload format, timeout behaviour, and method enforcement.

> **Developer action required:** run `make generate-proto` in `components/wp-operator/` to generate the Go gRPC stubs for `proto/gateway/v1/gateway.proto` before building the operator.

---

### Phase 5b: Update hello-world Example for HTTP Gateway ✅

Converts the `examples/hello-world` module from the `message-application` world to the `http-application` world, packages it as an OCI artifact, and wires it into the Tilt dev loop as a deployed `Application` CR with an HTTP trigger. This is the concrete deliverable backing the Phase 5 end-to-end test item.

#### Design decisions

- **OCI packaging:** WASM modules must be pushed as raw OCI artifact layers (not gzip-tar Docker image layers). The execution host's `oci::pull_wasm_bytes` calls `client.pull_blob` which returns the raw blob bytes; a `FROM scratch; COPY` Docker image would produce a gzip-compressed tarball layer and break compilation. `oras push` stores the file bytes directly as the layer blob. `oras` is invoked via its Docker image (`ghcr.io/oras-project/oras:v1.3.0`) with the workspace volume-mounted, so no separate CLI install is needed.
- **Tag strategy:** a mutable `:dev` tag is used for the local registry (`wasm-platform-registry.localhost:5001/hello-world:dev`). The `spec.module` field carries the tag reference; the execution host resolves it to a digest at load time, so cache keys remain correct across content changes.
- **Hot-reload signal:** changing wasm content without changing `spec.module` does not cause the operator to push an updated `ApplicationConfig` (no diff detected). The Tiltfile forces a reload by patching a `dev.wasm-platform/pushed-at` annotation on the ApplicationCR after each push; the operator sees a metadata change and re-sends config, causing the execution host to re-resolve the digest and load the new module.
- **Execution host Docker clean-up:** the `aot` Docker build stage and the `--build-context wasm=...` argument are leftovers from the pre-Phase-1 static module bake-in. They are removed here. The `precompile` binary is retained in `src/bin/precompile.rs` as a standalone dev tool but is no longer built as part of the execution-host Docker image.
- **Module reference format:** the CRD documents the format as `oci://<registry>/...` but `oci-distribution::Reference::from_str` follows Docker reference format and does not accept a URI scheme prefix. The Application CR must use a plain reference: `wasm-platform-registry.localhost:5001/hello-world:dev`. The `oci://` wording in the CRD description is aspirational; a scheme-stripping pass in `oci.rs` is a separate clean-up and is out of scope here.

#### Tasks

- [x] Update `examples/hello-world/src/lib.rs` — change `world: "message-application"` to `world: "http-application"` in the `wit_bindgen::generate!` call. Replace `impl Guest { fn on_message(...) }` with `fn on_request(request: HttpRequest) -> Result<HttpResponse, String>`. Return an `HttpResponse` with `status: 200` and a body summarising the request (e.g. `"hello from wasm: method=GET path=/hello"`).
- [x] Update `examples/hello-world/README.md` — change the world name, export description, and example table to match the new `on-request` handler. Note the `oras push` packaging requirement and the plain (scheme-free) registry reference format.
- [x] Create `examples/hello-world/k8s/application.yaml` — `Application` CR in namespace `wasm-platform`. Set `spec.module: wasm-platform-registry.localhost:5001/hello-world:dev`, `spec.http.path: /hello`, `spec.http.methods: [GET]`. Annotate with `dev.wasm-platform/pushed-at: "0"` as the initial value (Tilt will overwrite this).
- [x] Create `examples/hello-world/Tiltfile` — define a `hello_world(namespace, resource_deps=[])` function. Register three local resources: `hello-wasm-build` (`cargo build --manifest-path examples/hello-world/Cargo.toml --target wasm32-wasip2 --release`, watching `examples/hello-world/src` and `framework/runtime.wit`); `hello-wasm-push` (`docker run --rm -v $(pwd):/workspace -w /workspace ghcr.io/oras-project/oras:v1.3.0 push wasm-platform-registry.localhost:5001/hello-world:dev target/wasm32-wasip2/release/hello_world.wasm --artifact-type application/vnd.wasm.content.layer.v1+wasm`, depending on `hello-wasm-build` and `module-cache`); `hello-world-apply` (`kubectl apply -f examples/hello-world/k8s/application.yaml -n wasm-platform && kubectl annotate application hello-world -n wasm-platform dev.wasm-platform/pushed-at=$(date +%s) --overwrite`, depending on `hello-wasm-push` and wp-operator). Label all three `example`.
- [x] Update `components/execution-host/Tiltfile` — remove the `hello-wasm` local_resource entirely. Remove `target/wasm32-wasip2/release/hello_world.wasm` from the `custom_build` deps list. Remove `--build-context wasm=target/wasm32-wasip2/release` from the docker build command string.
- [x] Update `components/execution-host/Dockerfile` — remove the `aot` stage (the `FROM builder AS aot` block and its `COPY`/`RUN` lines). Remove `--bin precompile` from the `cargo build` command in the builder stage (and the `cp ... /build/precompile` line). Remove `COPY --from=aot ... /opt/wasm/hello_world.cwasm` from the runtime stage. Remove `ENV WASM_MODULE_PATH=...`.
- [x] Update root `Tiltfile` (`wasm-platform/Tiltfile`) — add `load('./examples/hello-world/Tiltfile', 'hello_world')` and call `hello_world(namespace, resource_deps=['wp-operator', 'execution-host', 'gateway'])` so the example app is deployed as part of `tilt up`.

> **Developer action required:** install the `oras` CLI (`brew install oras`) before running the dev loop. See `docs/contributions.md`.

---

### Phase 6: Host Function Implementations

Depends on Phase 1 (per-app routing provides the `ApplicationConfig` at invocation time).

- [ ] Implement `sql` interface in new `src/host_sql.rs` — `query`/`execute` backed by `tokio-postgres` using per-app `SqlConfig.connection_url`. Connection pools are per-application, lazily initialized, shared across invocations.
- [ ] Implement `kv` interface in new `src/host_kv.rs` — `get`/`set`/`delete` backed by `redis` crate using `KeyValueConfig.connection_url`, transparently prepending `KeyValueConfig.prefix` to all keys.
- [ ] Implement `messaging` interface in new `src/host_messaging.rs` — `send` publishes to NATS via the existing `async_nats::Client`.
- [ ] Wire into `Linker` — call `add_to_linker` for each WIT interface in `RuntimeState::new()`, implement generated `Host` traits on `HostState`.
- [ ] Update `HostState` — add per-invocation fields (`sql_config`, `kv_config`, `nats_client`) populated from the `ApplicationConfig` at invocation time.

### Phase 7: Sandboxing & Resource Limits

Parallel with Phases 3–6 (no dependency on per-app routing).

- [ ] Fuel metering — enable on `Engine`, set budget per `Store` before `on-message` (configurable via `WASM_FUEL_LIMIT`).
- [ ] Memory limits — configure `InstanceLimits` on `Engine` (e.g. 64 MB default, `WASM_MEMORY_LIMIT_MB`).
- [ ] Wall-clock timeout — wrap `spawn_blocking` in `tokio::time::timeout` (e.g. 30s default, `WASM_TIMEOUT_SECS`).
- [ ] Instance pooling (stretch goal) — `PoolingAllocationConfig` for pre-allocated slots; defer if per-invocation model performs adequately.

### Phase 8: README Alignment

Depends on Phases 3–7.

- [ ] Replace wildcard `fn.>` / `NATS_TOPIC_PREFIX` description with per-topic subscription model and internal prefix scheme.
- [ ] Replace `for_each_concurrent` reference with actual concurrency description.
- [ ] Verify module loading section matches Phase 1 implementation.
- [ ] Document gateway in execution-host README (NATS reply flow, platform JSON payload format, two-world dispatch).
- [ ] Document the two WIT worlds in the project README and `framework/` — note `message-application` for pure message-passing and `http-application` for HTTP endpoints.
- [ ] Full pass for any remaining stale claims.

### Verification

- [ ] Unit tests for `ModuleRegistry` — mock HTTP server for cache round-trip.
- [ ] Unit tests for host functions — fake external connections behind traits.
- [ ] Integration test — full stack in test namespace: create `Application` CR with `spec.http`, send HTTP request through gateway, verify guest executes with host functions and response returns.
- [ ] `cargo clippy` + `cargo fmt` pass.
- [ ] Helm chart deploys correctly with updated config.
