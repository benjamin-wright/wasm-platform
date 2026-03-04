# WASM-Platform: Architecture & Technology Guidance

## 1. Key Technology Decisions

### Runtime: Wasmtime or WasmEdge

| Runtime | Strengths | Best For |
|---|---|---|
| **Wasmtime** | Bytecode Alliance-backed, excellent WASI support, mature Cranelift JIT, strong security sandbox | Production-grade, spec-compliant environments |
| **WasmEdge** | Very fast cold-start (~0.5ms), built-in networking/async, WASI-NN for AI workloads | Ultra-low-latency FaaS with edge ambitions |

**Recommendation:** Start with **Wasmtime**. It has the strongest ecosystem, the most predictable WASI compatibility story, and Cranelift produces excellent machine code. WasmEdge is worth benchmarking later if sub-millisecond cold-start is a hard requirement.

### Control Plane Language: Rust or Go

- **Go** is the natural choice if your team has Kubernetes operator experience — the `controller-runtime` and `kubebuilder` SDKs make CRD management straightforward, and OCI distribution libraries (`oras-go`) are mature.
- **Rust** gives you tighter integration with Wasmtime (native Rust API), zero-cost host function abstractions, and the ability to share types between the control plane and the WASM host via `wit-bindgen`.

**Recommendation:** **Go for the control plane / operator**, **Rust for the WASM execution host**. This splits the problem cleanly — the operator manages Kubernetes lifecycle in Go where the ecosystem is strongest, and the execution host gets maximum performance and type safety in Rust where the WASM ecosystem is richest.

### Guest ↔ Host Interface: WASI + the Component Model

Use the **WebAssembly Component Model** with **WIT (WebAssembly Interface Types)** to define the contract between your guest modules and the host. This is the successor to WASI preview 1 and gives you:

- Strongly-typed, language-agnostic interfaces for your SQL and KV abstractions
- Capability-based security (guests can only call what the host explicitly provides)
- Composability (modules can be linked together)

Define your own WIT world:

```wit
package orchestrator:runtime;

interface sql {
    record row { columns: list<string>, values: list<string> }
    query: func(db: string, sql: string, params: list<string>) -> result<list<row>, string>;
    execute: func(db: string, sql: string, params: list<string>) -> result<u64, string>;
}

interface kv {
    get: func(store: string, key: string) -> result<option<list<u8>>, string>;
    set: func(store: string, key: string, value: list<u8>) -> result<_, string>;
    delete: func(store: string, key: string) -> result<bool, string>;
}

world application {
    import sql;
    import kv;
    export on-request: func(method: string, path: string, body: list<u8>) -> result<list<u8>, string>;
    export on-schedule: func(name: string) -> result<_, string>;
    export on-message: func(queue: string, payload: list<u8>) -> result<_, string>;
}
```

This is the single most important design choice — it defines your platform's API surface.

### OCI Distribution

Use **OCI artifacts** for module storage (your CRD already implies this with `oci://`). Libraries:

- Go: `oras.land/oras-go/v2`
- Rust: `oci-distribution` crate

Support content-addressable caching on each node so repeat cold-starts pull from local disk, not the registry.

---

## 2. Critical Design Considerations

### Cold-Start Budget

This is your primary competitive advantage. Target **< 5ms cold-start** (container-based FaaS is 100ms–10s). To achieve this:

1. **AOT compilation** — pre-compile `.wasm` → native code at deploy time, not at invocation time. Wasmtime supports serialised compiled modules.
2. **Instance pooling** — pre-allocate linear memory and table slots. Wasmtime's `PoolingAllocationConfig` lets you pre-reserve resources for N concurrent instances with copy-on-write memory initialization.
3. **Module caching** — compiled modules should be memory-mapped from disk. One compilation, many instantiations.

### Sandboxing & Multi-Tenancy

WASM is sandboxed by default, but your host functions break that sandbox deliberately (SQL, KV, network). Critical guardrails:

- **Fuel-based execution limits** — Wasmtime supports fuel metering. Set a fuel budget per invocation to prevent infinite loops or runaway computation.
- **Memory limits** — cap linear memory per instance (e.g., 64MB). Enforce in the `InstanceLimits` configuration.
- **Capability scoping** — a module should only access the databases and queues declared in its CRD `spec`. The host must enforce this by binding only the declared resources into the instance's imports.
- **Wall-clock timeouts** — fuel doesn't cover host calls. Wrap each invocation in an async timeout (e.g., 30s hard ceiling).

### Database Abstraction Layer

Don't build database engines — proxy to existing ones:

| CRD `kind` | Backing Implementation |
|---|---|
| `SQL` | Provision a logical database in a shared **PostgreSQL** cluster (or per-tenant if isolation requires it) |
| `KeyValue` | Namespace in **Redis** or **DragonflyDB**, or use embedded **SQLite** with WAL mode for single-node deployments |

The host functions translate the WIT `sql.query` / `kv.get` calls into actual client calls. This keeps the WASM module ignorant of the backing store.

### Event Trigger Architecture

Each trigger type needs a different ingestion path:

- **HTTP** — A lightweight reverse proxy (Envoy, or a custom Go/Rust HTTP gateway) routes by `spec.events[].route` → dispatches to the execution host. Keep the gateway stateless.
- **Schedule** — A cron controller watches CRDs and emits invocations at the specified schedule. Use Kubernetes `CronJob`-style leader election, or a lightweight library like `tokio-cron-scheduler` in Rust.
- **MessageQueue** — A consumer pool per queue (NATS JetStream, or RabbitMQ) that pulls messages and dispatches to the execution host. NATS is a strong choice here for its simplicity and built-in persistence.

### Graceful Scaling

Your execution hosts should be **stateless workers**. Scaling pattern:

- Scale on **concurrent invocations** (not CPU/memory), since WASM instances are tiny.
- A single execution host process can run thousands of concurrent WASM instances (they share the compiled module and use pooled memory).
- Use a Kubernetes HPA with a custom metric (active invocations / capacity) or KEDA for event-driven scaling.

---

## 3. Sensible Overall Architecture

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
│  │  • HTTP routing    │            └────────────────────┘           │
│  │  • Load balancing  │                                            │
│  └───────────────────┘                                            │
│                                                                    │
│  ┌───────────────────┐                                            │
│  │  Trigger Layer     │                                            │
│  │  • Cron scheduler  │                                            │
│  │  • MQ consumers    │                                            │
│  └───────────────────┘                                            │
└──────────────────────────────────────────────────────────────────┘
```

### Component Responsibilities

| Component | Language | Responsibility |
|---|---|---|
| **CRD Controller** | Go | Reconciles `Application` CRDs. Provisions databases, registers routes in the gateway, stores AOT-compiled modules in the cache. |
| **Execution Host** | Rust | Loads compiled modules, manages instance pools, exposes host functions (SQL, KV), executes invocations. Stateless — scales horizontally. |
| **Gateway** | Envoy / custom | Routes HTTP requests to execution hosts based on CRD route mappings. Health checks, rate limiting, TLS termination. |
| **Trigger Layer** | Go or Rust | Cron scheduler and message queue consumers that dispatch invocations to execution hosts via gRPC or an internal queue. |
| **Module Cache** | Filesystem | Per-node cache of OCI-pulled and AOT-compiled modules. Content-addressable by digest. |
| **Data Layer** | Managed services | PostgreSQL for SQL databases, Redis/Dragonfly for KV, NATS JetStream for message queuing. |

### Invocation Flow (HTTP)

1. Request arrives at Gateway → matched to route → forwarded to an Execution Host
2. Host looks up the module by application name → finds AOT-compiled module in cache
3. Host acquires a pre-allocated instance from the pool → binds host functions scoped to the application's declared databases
4. Host calls the guest's `on-request` export → guest runs, makes SQL/KV calls via imports → returns response
5. Host returns response to Gateway → instance is returned to the pool (memory is reset, not deallocated)

---

## 4. Suggested Phase Plan

| Phase | Scope | Milestone |
|---|---|---|
| **0 — Proof of Concept** | Single Rust binary. Load a `.wasm` file, expose SQL + KV host functions backed by SQLite + in-memory HashMap. HTTP trigger only. No Kubernetes. | Invoke a guest function over HTTP, read/write data. |
| **1 — Kubernetes Integration** | CRD + controller in Go. OCI pull. AOT compilation. Instance pooling. Deploy execution host as a Deployment. | `kubectl apply` a CRD → module is deployed and callable. |
| **2 — Multi-Trigger & Scaling** | Add schedule + message queue triggers. HPA or KEDA-based scaling. PostgreSQL + Redis backing. | Full CRD spec is functional. |
| **3 — Multi-Tenancy & Production** | Fuel metering, memory limits, per-tenant resource isolation, observability (OpenTelemetry traces per invocation), CLI tooling, SDK for guest authors. | Production-ready for internal workloads. |

---

## Key Takeaway

**The highest-leverage early investment is defining the WIT interface.** It's your platform's API — every guest module will be compiled against it, and changing it later is a breaking change for all users. Get the SQL, KV, and event handler signatures right in Phase 0, and the rest of the system is an implementation detail behind a stable contract.
