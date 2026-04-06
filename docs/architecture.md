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

The platform defines its own WIT world in [`framework/runtime.wit`](../framework/runtime.wit) — the source of truth for the platform's API surface. Two worlds are defined: `message-application` (binary payload in/out) and `http-application` (typed HTTP request/response).

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
| **Redis** | Single shared instance. Per-app key-prefix isolation (`<namespace>/<spec.keyValue>/`). |
| **NATS** | Single shared instance. Per-app subject isolation (operator-assigned prefixed topics). |

**Connection model:** execution hosts are configured once with shared infrastructure coordinates (`PG_HOST`/`PG_PORT`, `REDIS_URL` env vars). The `ConfigSync` service carries only the per-app delta: database name, username, and password for PostgreSQL; key prefix for Redis. PostgreSQL uses **per-app connection pools** keyed by `(database_name, username)`, lazily initialized. Redis uses a **single multiplexed connection** — isolation is purely by key prefix.

The host functions translate WIT `sql.query` / `kv.get` calls into actual client calls, keeping WASM modules ignorant of the backing store.

### Event Trigger Architecture

- **HTTP** — the gateway serialises requests into a platform-private NATS payload and publishes to the app's `http.<namespace>.<name>` subject. The execution host calls the module's `on-request` export with typed WIT records and returns the response via NATS reply.
- **MessageQueue** — the execution host subscribes to per-app `fn.`-prefixed NATS subjects and calls the module's `on-message` export.
- **Schedule** — (planned) a cron controller watches CRDs and emits invocation events to NATS.

### Config Sync & Scaling

Execution hosts are deployed as a **Deployment**. The wp-operator pushes configuration via gRPC: full snapshot on startup/desync, incremental deltas ongoing. On each new config, the execution host checks the module cache for a precompiled `.cwasm`; on a miss, pulls the OCI artifact, AOT-compiles it, and pushes the result back.

Scaling targets concurrent invocations (not CPU/memory). A single execution host can run thousands of concurrent WASM instances. HPA with a custom metric or KEDA for event-driven scaling.

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
│  │  Trigger Layer       │      │  │  Key-prefix isolation        │ │ │
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

