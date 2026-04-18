# Architecture

Technology decisions, system design, and design constraints for wasm-platform. For coding conventions, see [standards.md](standards.md). For project overview and quick-start, see the [README](../README.md).

---

## 1. Key Technology Decisions

### Runtime: Wasmtime

| Runtime | Strengths | Best For |
|---|---|---|
| **Wasmtime** | Bytecode Alliance-backed, excellent WASI support, mature Cranelift JIT, strong security sandbox | Production-grade, spec-compliant environments |
| **WasmEdge** | Very fast cold-start (~0.5ms), built-in networking/async, WASI-NN for AI workloads | Ultra-low-latency FaaS with edge ambitions |

**Decision:** Wasmtime. Strongest ecosystem, most predictable WASI compatibility, excellent Cranelift code generation. WasmEdge is worth benchmarking later if sub-millisecond cold-start becomes a hard requirement.

### Control Plane Language: Rust + Go

**Decision:** Go for the Kubernetes operator (where `controller-runtime` and `kubebuilder` are strongest), Rust for the execution host and gateway (where Wasmtime's native API, `wit-bindgen` type sharing, and zero-cost abstractions matter most).

### Guest ↔ Host Interface: WASI + the Component Model

The **WebAssembly Component Model** with **WIT (WebAssembly Interface Types)** defines the contract between guest modules and the host:

- Strongly-typed, language-agnostic interfaces for SQL and KV abstractions
- Capability-based security (guests can only call what the host explicitly provides)
- Composability (modules can be linked together)

The platform defines its own WIT world in [`framework/runtime.wit`](../framework/runtime.wit) — the source of truth for the platform's API surface. Two worlds are defined:

- **`message-application`** — exports `on-message: func(payload: list<u8>)`. For apps triggered by NATS subjects.
- **`http-application`** — exports `on-request` with typed `http-request` / `http-response` records, giving module authors a clean interface with no manual parsing.

The WASI HTTP resource model is explicitly not used; custom records are simpler to implement on the host and sufficient for a buffered FaaS model.

Both worlds import a `log` interface. Guests emit structured log entries via explicit levels (`debug`, `info`, `warn`, `error`); the host forwards to `tracing` with per-app labels. A custom `log` interface is preferred over wiring WASI stdout/stderr so that level and per-app attribution are available without parsing.

### OCI Distribution

OCI artifacts for module storage. Libraries: `oras.land/oras-go/v2` (Go), `oci-distribution` crate (Rust). A centralized module cache stores AOT-compiled artifacts keyed by `(digest, arch, wasmtime_version)`.

---

## 2. Critical Design Considerations

### Cold-Start Budget

Target **< 5ms cold-start** (container-based FaaS is 100ms–10s):

1. **AOT compilation** — `.wasm` → native code at deploy time via `engine.precompile_component()`.
2. **Instance pooling** — `PoolingAllocationConfig` pre-reserves memory/table slots with copy-on-write initialization.
3. **Module caching** — compiled modules are memory-mapped from disk. One compilation, many instantiations.

### Sandboxing & Multi-Tenancy

WASM is sandboxed by default; host functions break that sandbox deliberately. Guardrails:

- **Fuel metering** — prevents infinite loops / runaway computation.
- **Memory limits** — cap linear memory per instance (e.g. 64 MB) via `InstanceLimits`.
- **Capability scoping** — modules access only the databases and queues declared in their CRD `spec`.
- **Wall-clock timeouts** — fuel doesn't cover host calls; each invocation is wrapped in an async timeout.

### Data Layer

Three shared backing stores, all provisioned by the **[db-operator](https://github.com/benjamin-wright/db-operator)** via CRDs in the platform Helm chart:

| Store | Isolation Model |
|---|---|
| **PostgreSQL** | Per-app logical database + dedicated user, created by the wp-operator. |
| **Redis** | Single shared instance. Per-app key-prefix isolation (`<namespace>/<app>/`), assigned automatically — no CRD field required. |
| **NATS** | Single shared instance. Per-app subject isolation (operator-assigned prefixed topics). |

**Connection model:** execution hosts are configured once with shared infrastructure coordinates (`PG_HOST`/`PG_PORT`, `REDIS_URL` env vars). The `ConfigSync` service carries only the per-app delta: database name, username, and password for PostgreSQL. PostgreSQL uses **per-app connection pools** keyed by `(database_name, username)`, lazily initialized. Redis uses a **single multiplexed connection** — isolation is purely by key prefix, automatically derived from `(namespace, app)`.

The host functions translate WIT `sql.query` / `kv.get` calls into actual client calls, keeping WASM modules ignorant of the backing store.

### Event Trigger Architecture

- **HTTP** — the gateway serialises the incoming HTTP request as a platform-private JSON object and publishes it to the app's `http.<namespace>.<name>` NATS subject (auto-generated by the wp-operator; users set only `spec.trigger: http`). The execution host decodes the payload and calls `on-request` with properly typed WIT records — the module never sees JSON. The response is serialised back to JSON for the NATS reply. The internal format is opaque to platform users; a future auth middleware layer can inject headers (e.g. `x-user-id`) into the payload before publish.
- **MessageQueue** — the execution host subscribes to per-app NATS subjects prefixed with `fn.`. The wp-operator prepends `fn.` to the user-supplied `spec.topic` before pushing config (e.g. `my-app.events` → NATS subject `fn.my-app.events`). Topic uniqueness is enforced cluster-wide on the user-supplied value before prefixing; the `fn.` and `http.` prefix classes are disjoint, so uniqueness is only checked within the same class.
- **Schedule** — (planned) a cron controller watches CRDs and emits invocation events to NATS.

### Config Sync & Scaling

Execution hosts are deployed as a **Deployment**. The wp-operator pushes configuration via gRPC: full snapshot on startup/desync, incremental deltas ongoing. On each new config, the execution host checks the module cache for a precompiled `.cwasm`; on a miss, pulls the OCI artifact, AOT-compiles it, and pushes the result back.

Scaling targets concurrent invocations (not CPU/memory). A single execution host can run thousands of concurrent WASM instances. HPA with a custom metric or KEDA for event-driven scaling.

**NATS queue groups:** Each execution host replica subscribes via `queue_subscribe` using the topic name as the queue group. NATS delivers each message to exactly one replica in the group, matching the Kafka consumer-group pattern. This prevents duplicate invocations during horizontal scaling and rolling updates. Old replicas that are draining leave the queue group naturally when their NATS connection closes.

---

## 3. System Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│                          Kubernetes Cluster                          │
│                                                                      │
│  ┌──────────────────┐ gRPC push     ┌──────────────────────────────┐ │
│  │  wp-operator      │──────────────▶│  Execution Host (Deployment) │ │
│  │  (Go, kubebuilder)│◀ ─ ─ ─ ─ ─ ─ │                              │ │
│  │                   │ gRPC full cfg │  ┌──────────────────────┐   │ │
│  │  • Watches        │  (on startup/ │  │      Pod (×N)        │   │ │
│  │    Application    │   desync)     │  │  ┌────────────────┐  │   │ │
│  │    CRDs           │               │  │  │ execution-host │  │   │ │
│  │  • Manages DBs,   │               │  │  │                │  │   │ │
│  │    users, creds   │               │  │  │ Wasmtime Pool  │  │   │ │
│  │    in shared PG   │               │  │  │                │  │   │ │
│  │  • Registers      │               │  │  │ Host Fn Layer  │  │   │ │
│  │    routes/triggers│               │  │  └───────┬────────┘  │   │ │
│  │  • gRPC ConfigSync│               │  └──────────┼───────────┘   │ │
│  └──────────────────┘               └─────────────┼───────────────┘ │
│                         ┌───────────◀─────────────┘                  │
│                         │     check / push compiled artifact          │
│                         ▼                                             │
│  ┌──────────────────────────────┐                                    │
│  │  Module Cache (centralized)  │                                    │
│  │  • Keyed by digest, arch,    │                                    │
│  │    Wasmtime version          │                                    │
│  │  • Execution hosts pull from │                                    │
│  │    OCI registry on miss,     │                                    │
│  │    AOT-compile, then push    │                                    │
│  └──────────────────────────────┘                                    │
│                                                                      │
│  ┌─────────────────────┐      ┌────────────────────────────────────┐ │
│  │  Gateway (Rust)      │      │  Shared Data Layer                 │ │
│  │  • HTTP → NATS       │      │  ┌──────────────────────────────┐ │ │
│  │    translation       │      │  │  PostgreSQL (single shared)  │ │ │
│  └─────────────────────┘      │  │  Per-app DB + user            │ │ │
│                                │  ├──────────────────────────────┤ │ │
│  ┌─────────────────────┐      │  │  Redis (single shared)       │ │ │
│  │  Trigger Layer       │      │  │  Auto <ns>/<app>/ prefix     │ │ │
│  │  • Cron scheduler    │      │  ├──────────────────────────────┤ │ │
│  └─────────────────────┘      │  │  NATS (single shared)        │ │ │
│                                │  │  Subject-prefix isolation    │ │ │
│                                │  └──────────────────────────────┘ │ │
│                                └────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────┘
```

### Component Responsibilities

| Component | Language | Responsibility |
|---|---|---|
| **WP Operator** | Go | Reconciles `Application` CRDs. Manages per-app PG databases/users. Pushes per-app credentials and config to execution hosts and routes to the gateway via gRPC. Deferred-delete of databases via Redis reference counting with TTL. |
| **Execution Host** | Rust | Syncs config from operator. Loads modules via the module cache. Subscribes to per-app NATS subjects. Executes WASM modules with scoped host functions (SQL, KV, messaging). |
| **Gateway** | Rust | Translates HTTP requests to NATS request-reply based on operator-pushed route table. Method enforcement, timeout handling. |
| **Module Cache** | Rust | Stores and serves AOT-compiled module artifacts keyed by `(digest, arch, wasmtime_version)`. |
| **Token Service** | TBD | Separately scalable service for minting JWT tokens. |
| **Trigger Layer** | TBD | Cron scheduler dispatching invocation events to NATS. |
| **Data Layer** | Managed | PostgreSQL, Redis, NATS — all provisioned by [db-operator](https://github.com/benjamin-wright/db-operator) via CRDs. |

### Invocation Flow (HTTP)

1. Request arrives at gateway → serialised as platform JSON → published to `http.<namespace>.<name>` NATS subject with a reply subject.
2. Execution host looks up the app config by subject → retrieves the precompiled `Component`.
3. Execution host instantiates the module → binds scoped host functions → calls `on-request` with typed WIT records.
4. Guest runs, may call `sql`/`kv`/`messaging` imports → returns an `HttpResponse`.
5. Execution host serialises the response as JSON → publishes to NATS reply subject → gateway constructs HTTP response.

---

## 4. Decisions (Phase 8+)

### Multi-Function Application CRD Shape

**Decision:** Replace `spec.module`, `spec.topic`, and `spec.http` with a `spec.functions` list. Each entry has:

- `name` — identifier unique within the Application.
- `module` — OCI image reference for the `.wasm` module.
- `trigger` — exactly one of `trigger.http` (`HttpConfig`) or `trigger.topic` (string); enforced by CEL validation, consistent with the existing pattern.

Application-level fields (`spec.env`, `spec.sql`, `spec.keyValue`) are retained and remain shared across all functions in the Application. The CRD is v1alpha1, so this is a clean breaking migration; no backwards-compatibility shim is provided. The hello-world Application CR is migrated to the new single-entry `spec.functions` shape in Phase 8.2.

**Uniqueness:** topic uniqueness remains cluster-wide per the existing `TopicConflict` enforcement. HTTP path uniqueness is enforced the same way. Uniqueness is checked against the user-supplied value at the function level, not the application level.

---

### `spec.metrics` Schema and Validation

**Decision:** `spec.metrics` is a list of metric definitions. Each entry:

- `name` — Prometheus metric name; must match `[a-zA-Z_:][a-zA-Z0-9_:]*`, max 64 characters, must not start with `__` (Prometheus reserved prefix).
- `type` — enum: `counter` or `gauge`.
- `labels` — list of Prometheus label key strings; each must match `[a-zA-Z_][a-zA-Z0-9_]*`, max 10 entries per metric, must not include `app_name` or `app_namespace` (host-injected labels).

`spec.metrics` names must be unique within a single Application (enforced by CEL). Cross-Application uniqueness is enforced by the operator (see below). A metric `name` is the globally unique identifier — `type` and `labels` are not part of the uniqueness key.

---

### Metric Name Uniqueness Enforcement Strategy

**Decision:** Reconciler-time validation — no admission webhook.

The operator checks all existing Applications for metric name collisions at reconcile time. The Application with the oldest `creationTimestamp` owns each metric name (tiebreak: lexicographically lower `namespace/name`). An Application whose metric names collide with an existing owner enters a `MetricConflict` condition (`Ready: False`). When the owner is deleted or removes the conflicting metric name, blocked Applications are re-evaluated automatically on the next reconcile.

**Rationale:** Consistent with the existing `TopicConflict` pattern. No additional infrastructure (cert-manager, webhook configuration) is required. Reconciler-time feedback (seconds) is sufficient for a v1alpha1 API — the cost of delayed feedback is low compared to the cost of adding and maintaining webhook infrastructure.

---

### Migrations Contract

**Decision:**

- **Image reference:** `spec.migrations.image` (optional OCI image reference) at the Application level. The image is expected to run to completion (exit 0 = success). No arguments are passed by the platform; the image is responsible for connecting to its own database using credentials injected as environment variables by the operator (`PG_HOST`, `PG_PORT`, `PG_DATABASE`, `PG_USER`, `PG_PASSWORD`).
- **Trigger:** A migrations Job is created on the first apply of any Application that has `spec.migrations.image` set, and on any subsequent apply where `spec.migrations.image` or any `spec.functions[*].module` digest changes (detected via `metadata.generation` increment). Job name pattern: `<app-name>-migrations-<generation>`.
- **Activation gate:** The operator does not push an ApplicationConfig to execution hosts until the migrations Job for the current generation has completed successfully. Applications without `spec.migrations.image` are unaffected.
- **Failure model:** If the Job fails (all retries exhausted), the operator sets `MigrationFailed: True` on the Application status and does not push config. No traffic flows to the Application. The user fixes the migrations image and re-applies, incrementing `metadata.generation` and triggering a new Job.
- **Rollback:** Out of scope for v1alpha1. Migrations are forward-only.
- **Job retention:** Jobs are retained after completion for debugging. A completed Job for a prior generation is deleted when a new generation's Job is created.

---

### Config-Sync Proto Changes for Multi-Function and Metrics

**Decision:** Clean break — field numbers are reassigned for clarity. Since the operator and execution host are always deployed together and the API is v1alpha1, no wire-format backwards compatibility is required.

`ApplicationConfig` is restructured:

| Field number | Name | Description |
|---|---|---|
| 1 | `name` | unchanged |
| 2 | `namespace` | unchanged |
| 3 | `functions` | `repeated FunctionConfig` — replaces `module_ref` (old 3), `topic` (old 4), `world_type` (old 8), `http` (old 9) |
| 4 | `env` | `map<string, string>` — was field 5 |
| 5 | `sql` | `optional SqlConfig` — was field 6 |
| 6 | `key_value` | `optional KeyValueConfig` — was field 7 |
| 7 | `metrics` | `repeated MetricDefinition` — new |

New messages and enums added:

- **`FunctionConfig`** — `name`, `module_ref`, `world_type` (`WorldType`), `topic` (optional string), `http_config` (optional `HttpConfig`).
- **`MetricDefinition`** — `name`, `type` (`MetricType`), `label_keys` (`repeated string`).
- **`MetricType`** enum — `METRIC_TYPE_COUNTER = 0`, `METRIC_TYPE_GAUGE = 1`.

