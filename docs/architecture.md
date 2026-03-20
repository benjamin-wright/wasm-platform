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

Content-addressable caching on each node so repeat cold-starts pull from local disk, not the registry.

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
| `SQL` | Logical database in a shared **PostgreSQL** cluster (or per-tenant if isolation requires it) |
| `KeyValue` | Namespace in **Redis** or **DragonflyDB**, or embedded **SQLite** with WAL mode for single-node deployments |

The host functions translate the WIT `sql.query` / `kv.get` calls into actual client calls. This keeps the WASM module ignorant of the backing store.

Supporting databases (PostgreSQL, Redis, etc.) are deployed and managed via the **[db-operator](https://github.com/benjamin-wright/db-operator)** — a custom Kubernetes operator for provisioning and lifecycle management of the backing data stores.

### Event Trigger Architecture

Each trigger type needs a different ingestion path:

- **HTTP** — A lightweight HTTP server translates requests to event subjects by `spec.events[].route`. Keep the gateway stateless.
- **Schedule** — A cron controller watches CRDs and emits invocations at the specified schedule. Use Kubernetes `CronJob`-style leader election, or `tokio-cron-scheduler` in Rust.
- **MessageQueue** — A consumer pool per queue (NATS JetStream or RabbitMQ) that pulls messages and dispatches to the execution host. NATS is a strong choice for its simplicity and built-in persistence.

### Graceful Scaling

Execution hosts are **stateless workers**. Scaling pattern:

- Scale on **concurrent invocations** (not CPU/memory), since WASM instances are tiny.
- A single execution host process can run thousands of concurrent WASM instances (they share the compiled module and use pooled memory).
- Use a Kubernetes HPA with a custom metric (active invocations / capacity) or KEDA for event-driven scaling.

---

## 3. System Architecture

```
┌──────────────────────────────────────────────────────────────────┐
│                        Kubernetes Cluster                        │
│                                                                  │
│  ┌─────────────────┐    ┌──────────────────────────────────────┐ │
│  │  CRD Controller  │    │         Execution Hosts (Rust)       │ │
│  │  (Go, kubebuilder)│    │                                      │ │
│  │                   │    │  ┌────────┐ ┌────────┐ ┌────────┐  │ │
│  │  • Watches        │    │  │Wasmtime│ │Wasmtime│ │Wasmtime│  │ │
│  │    Application    │    │  │Instance│ │Instance│ │Instance│  │ │
│  │    CRDs           │    │  │ Pool   │ │ Pool   │ │ Pool   │  │ │
│  │  • Provisions DBs │    │  └───┬────┘ └───┬────┘ └───┬────┘  │ │
│  │  • Registers      │    │      │          │          │        │ │
│  │    routes/triggers │    │  ┌───┴──────────┴──────────┴────┐  │ │
│  │  • Pulls & AOT    │    │  │     Host Function Layer       │  │ │
│  │    compiles modules│    │  │  (SQL proxy, KV proxy, etc.)  │  │ │
│  └────────┬──────────┘    │  └──────────────┬────────────────┘  │ │
│           │               └─────────────────┼────────────────────┘ │
│           │                                 │                      │
│  ┌────────▼──────────┐            ┌─────────▼─────────┐           │
│  │  Module Cache      │            │  Data Layer        │           │
│  │  (OCI + AOT disk   │            │  ┌──────────────┐  │           │
│  │   cache per node)  │            │  │  PostgreSQL   │  │           │
│  └───────────────────┘            │  │  (SQL dbs)    │  │           │
│                                   │  ├──────────────┤  │           │
│  ┌───────────────────┐            │  │  Redis/NATS   │  │           │
│  │  Gateway (Envoy    │            │  │  (KV + MQ)    │  │           │
│  │  or custom)        │            │  └──────────────┘  │           │
│  │  • HTTP translation│            └────────────────────┘           │
│  └───────────────────┘                                            │
│                                                                    │
│  ┌───────────────────┐                                            │
│  │  Trigger Layer     │                                            │
│  │  • Cron scheduler  │                                            │
│  └───────────────────┘                                            │
└──────────────────────────────────────────────────────────────────┘
```

### Component Responsibilities

| Component | Language | Responsibility |
|---|---|---|
| **CRD Controller** | Go | Reconciles `Application` CRDs. Provisions databases, registers routes in the gateway, stores AOT-compiled modules in the cache. |
| **Execution Host** | Rust | Listens for NATS messages, loads compiled modules, manages instance pools, exposes host functions (SQL, KV), executes invocations. Stateless — scales horizontally. |
| **Gateway** | Go or Rust | Translates HTTP requests to NATS events based on CRD route mappings. Health checks, rate limiting, TLS termination, auth checks. |
| **Token Service** | Go or Rust | Separately scalable service for minting JWT tokens for auth purposes. |
| **Trigger Layer** | Go or Rust | Cron scheduler that dispatches invocation events to NATS. |
| **Module Cache** | Filesystem | Per-node cache of OCI-pulled and AOT-compiled modules. Content-addressable by digest. |
| **Data Layer** | Managed services | PostgreSQL for SQL databases, Redis/Dragonfly for KV, NATS JetStream for message queuing. |

### Invocation Flow (HTTP)

1. Request arrives at Gateway → matched to route → forwarded to a NATS subject.
2. Host looks up the module by application name → finds AOT-compiled module in cache.
3. Host acquires a pre-allocated instance from the pool → binds host functions scoped to the application's declared databases.
4. Host calls the guest's `on-request` export → guest runs, makes SQL/KV calls via imports → returns response.
5. Host returns response to Gateway → instance is returned to the pool (memory is reset, not deallocated).


