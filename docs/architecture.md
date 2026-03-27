# Architecture

Technology decisions, system design, and design constraints for wasm-platform. For coding conventions, see [standards.md](standards.md). For project overview and quick-start, see the [README](../README.md).

---

## 1. Key Technology Decisions

### Runtime: Wasmtime

| Runtime | Strengths | Best For |
|---|---|---|
| **Wasmtime** | Bytecode Alliance-backed, excellent WASI support, mature Cranelift JIT, strong security sandbox | Production-grade, spec-compliant environments |
| **WasmEdge** | Very fast cold-start (~0.5ms), built-in networking/async, WASI-NN for AI workloads | Ultra-low-latency FaaS with edge ambitions |

**Decision:** Wasmtime. It has the strongest ecosystem, the most predictable WASI compatibility story, and Cranelift produces excellent machine code. WasmEdge is worth benchmarking later if sub-millisecond cold-start becomes a hard requirement.

### Control Plane Language: Rust + Go

- **Go** is the natural choice for Kubernetes operators — `controller-runtime` and `kubebuilder` SDKs make CRD management straightforward, and OCI distribution libraries (`oras-go`) are mature.
- **Rust** gives tighter integration with Wasmtime (native API), zero-cost host function abstractions, and shared types between the control plane and the WASM host via `wit-bindgen`.

**Decision:** Go for the control plane / operator, Rust for the WASM execution host. The operator manages Kubernetes lifecycle in Go where the ecosystem is strongest; the execution host gets maximum performance and type safety in Rust where the WASM ecosystem is richest.

### Guest ↔ Host Interface: WASI + the Component Model

The **WebAssembly Component Model** with **WIT (WebAssembly Interface Types)** defines the contract between guest modules and the host. This gives:

- Strongly-typed, language-agnostic interfaces for SQL and KV abstractions
- Capability-based security (guests can only call what the host explicitly provides)
- Composability (modules can be linked together)

The platform defines its own WIT world in [`framework/runtime.wit`](../framework/runtime.wit) — this is the single most important design choice, as it defines the platform's API surface. The WIT file is the source of truth; refer to it directly.

### OCI Distribution

OCI artifacts for module storage. Libraries:

- Go: `oras.land/oras-go/v2`
- Rust: `oci-distribution` crate

Content-addressable centralized cache for AOT-compiled modules. On a cache miss, the execution host pulls the raw OCI artifact, AOT-compiles it, and pushes the result back to the cache so all execution hosts can benefit.

---

## 2. Critical Design Considerations

### Cold-Start Budget

Target **< 5ms cold-start** (container-based FaaS is 100ms–10s). To achieve this:

1. **AOT compilation** — pre-compile `.wasm` → native code at deploy time, not at invocation time. Wasmtime supports serialised compiled modules.
2. **Instance pooling** — pre-allocate linear memory and table slots. Wasmtime's `PoolingAllocationConfig` pre-reserves resources for N concurrent instances with copy-on-write memory initialization.
3. **Module caching** — compiled modules are memory-mapped from disk. One compilation, many instantiations.

### Sandboxing & Multi-Tenancy

WASM is sandboxed by default, but host functions break that sandbox deliberately (SQL, KV, network). Critical guardrails:

- **Fuel-based execution limits** — Wasmtime fuel metering prevents infinite loops or runaway computation.
- **Memory limits** — cap linear memory per instance (e.g. 64 MB) via `InstanceLimits`.
- **Capability scoping** — a module should only access the databases and queues declared in its CRD `spec`. The host enforces this by binding only the declared resources into the instance's imports.
- **Wall-clock timeouts** — fuel doesn't cover host calls. Wrap each invocation in an async timeout (e.g. 30s hard ceiling).

### Database Abstraction Layer

Proxy to existing database engines — don't build new ones:

| CRD `kind` | Backing Implementation |
|---|---|
| `SQL` | Per-application logical database and dedicated user inside the **single shared PostgreSQL** instance. The wp-operator creates the database, user, and grants. Credentials are delivered to execution hosts via the gRPC `ConfigSync` service alongside the rest of the app config. |
| `KeyValue` | Key-prefixed isolation in the **single shared Redis** instance. The execution host prepends the application's declared prefix to every key it reads or writes. |

The host functions translate the WIT `sql.query` / `kv.get` calls into actual client calls. This keeps the WASM module ignorant of the backing store.

The shared PostgreSQL, Redis, and NATS instances are deployed as part of the platform — the wp-operator does **not** need an external db-operator to provision databases. Instead, it connects directly to the shared PostgreSQL instance and manages databases, users, and permissions itself.

### Event Trigger Architecture

Each trigger type needs a different ingestion path:

- **HTTP** — A lightweight HTTP server translates requests to NATS messages. The gateway serialises the HTTP context (method, path, headers, body) into a payload and publishes it to the application's NATS subject. The execution host delivers the payload to the module's `on-message` export and forwards any returned response bytes back to the caller.
- **Schedule** — A cron controller watches CRDs and emits invocations at the specified schedule. Use Kubernetes `CronJob`-style leader election, or `tokio-cron-scheduler` in Rust.
- **MessageQueue** — A consumer pool per queue on the **single shared NATS** instance. Applications are isolated by NATS subject prefix; the execution host subscribes only to the subjects declared for each application. NATS JetStream is the strong choice for built-in persistence.

### Graceful Scaling

Execution hosts are deployed as a **Deployment**. The wp-operator communicates with execution hosts over **gRPC** using a hybrid sync model: on startup or when desynced, a host requests the full current configuration; afterwards the operator streams incremental configuration deltas to connected hosts. See the [wp-operator Config API](../components/wp-operator/README.md#config-api) for the service definition.

When a new config is received, each execution host checks the centralized module cache for a precompiled artifact; on a miss it pulls the OCI artifact, AOT-compiles it, and writes the result back to the cache. Scaling pattern:

- Scale on **concurrent invocations** (not CPU/memory), since WASM instances are tiny.
- A single execution host process can run thousands of concurrent WASM instances (they share the compiled module and use pooled memory).
- Use a Kubernetes HPA with a custom metric (active invocations / capacity) or KEDA for event-driven scaling.

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
│  │  Gateway (Envoy      │      │  Shared Data Layer                 │ │
│  │  or custom)          │      │  ┌──────────────────────────────┐ │ │
│  │  • HTTP translation  │      │  │  PostgreSQL (single shared)  │ │ │
│  └─────────────────────┘      │  │  Per-app DB + user managed    │ │ │
│                                │  │  by wp-operator               │ │ │
│  ┌─────────────────────┐      │  ├──────────────────────────────┤ │ │
│  │  Trigger Layer       │      │  │  Redis (single shared)       │ │ │
│  │  • Cron scheduler    │      │  │  Isolated by key prefix      │ │ │
│  └─────────────────────┘      │  ├──────────────────────────────┤ │ │
│                                │  │  NATS (single shared)        │ │ │
│                                │  │  Isolated by subject prefix  │ │ │
│                                │  └──────────────────────────────┘ │ │
│                                └────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────┘
```

### Component Responsibilities

| Component | Language | Responsibility |
|---|---|---|
| **WP Operator** | Go | Reconciles `Application` CRDs. Creates and manages per-application databases, users, and permissions inside the shared PostgreSQL instance. Passes database credentials to execution hosts via the gRPC `ConfigSync` service alongside app configs. Registers routes in the gateway. |
| **Execution Host** | Rust | Deployed as a Deployment. Syncs configuration (including per-app database credentials) from the wp-operator via gRPC on startup and as changes occur. On each new config, checks the module cache for a precompiled artifact; on a miss, pulls the OCI artifact, AOT-compiles it, and pushes the result back to the cache. Listens for NATS messages (isolated by subject prefix), manages instance pools, exposes host functions (SQL with per-app credentials, KV with per-app key prefix), executes invocations. |
| **Gateway** | Go or Rust | Translates HTTP requests to NATS events based on CRD route mappings. Health checks, rate limiting, TLS termination, auth checks. |
| **Token Service** | Go or Rust | Separately scalable service for minting JWT tokens for auth purposes. |
| **Trigger Layer** | Go or Rust | Cron scheduler that dispatches invocation events to NATS. |
| **Module Cache** | Rust | Centralized cache service. Stores and retrieves AOT-compiled module artifacts keyed by digest, architecture, and Wasmtime version. Execution hosts check the cache on config load, and push newly compiled artifacts back after a cache miss. |
| **Data Layer** | Managed services | Single shared PostgreSQL instance (per-app databases managed by wp-operator), single shared Redis (per-app key-prefix isolation), single shared NATS (per-app subject-prefix isolation). |

### Invocation Flow (HTTP)

1. Request arrives at Gateway → serialised into a NATS message payload (method, path, headers, body) → published to the application's NATS subject.
2. Host looks up the module by application name → retrieves the AOT-compiled artifact from the module cache (already loaded at config time).
3. Host acquires a pre-allocated instance from the pool → binds host functions scoped to the application's declared databases.
4. Host calls the guest's `on-message` export with the payload → guest runs, makes SQL/KV/messaging calls via imports → optionally returns response bytes.
5. Host forwards response bytes (if any) back to Gateway → instance is returned to the pool (memory is reset, not deallocated).

