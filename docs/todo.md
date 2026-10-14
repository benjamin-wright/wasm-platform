# TODO

Active implementation plan for the wasm-platform project.

Use the [architecture proposal](proposal.md) for target responsibility boundaries and ownership invariants while executing this plan. Task order and remediation gates remain here.

Finding IDs refer to the supplied Wasm platform technical review. F01–F11 are P1; F12 combines P1 correctness with P2 resilience; F13–F18 are P2. These are engineering priorities.

## Delivery order and readiness gates

Preserve the Rust/Wasmtime execution host, Go operator, WIT contracts, OCI distribution, and NATS dispatch. The next milestone is reliable execution and isolation under updates, failures, and load. Follow the stage 0 migration plan to deliver remediation stages 1–4 before resuming Phases 13–16 or expanding the CLI. Pair each repair with its regression test and use the [local Tilt CI gate](contributions.md#local-ci).

| Order | Milestone | Required outcome |
|---|---|---|
| 0 | Apply the architecture migration plan | Agreed policies and 17 implementation slices guide delivery of stages 1–4. |
| 1 | Contain execution and trust risks | Timed-out work stops; task records are reaped; credentials, compiled artifacts, and SQL policy have enforceable boundaries. |
| 2 | Make desired state converge | Immutable generations, replayable configuration, owned routes, rotation, and migration fencing survive updates and reconnects. |
| 3 | Bound failure and shutdown | Finite resource/deadline budgets, correct HTTP and guest configuration, and defined message/drain semantics. |
| 4 | Verify the development and release path | Reproducible CI, failure-mode regressions, fresh-clone examples, and accurate documentation. |

- **Trusted local prototype:** restrict authorship and service reachability to trusted participants. Verify fresh builds and tests; fix execution/task lifetime, stale build inputs, and setup instructions before calling it operational.
- **Shared internal pilot:** require every P1 repair, stages 1–4, tested credential rotation, dependency outage/restart recovery, migration and event-delivery policies, and a working CI gate. Keep configuration serving single-authority until stage 2 proves otherwise.
- **External or mutually untrusted users:** additionally require a documented tenant threat model, workload identity and least-privilege authorization, SQL/metric isolation, quotas, secure registry policy, and negative security tests. Define whether guests may publish to any platform topic or only authorized topics; enforce the chosen policy end to end.
- **Production:** additionally require workload-specific load/soak/failover tests, alerts and runbooks, backup/restore tests, release compatibility, upgrade/rollback procedures, and dependency/security maintenance.

### Verification

Use deterministic clocks, controllable loaders, and barriers for ordering tests; use a disposable cluster for database, credential, routing, and rollout behavior. Tests must exercise the production component/dispatch path and use finite client deadlines. Primitive fuel/memory tests or successful CR updates alone do not establish the platform guarantees. Report results in the task conversation, following [AGENTS.md](../AGENTS.md).

Keep existing `demo-app`, `counter-app`, and `sql-hello` fixtures passing. Follow the definition of done in [standards.md](standards.md): changed external behavior requires e2e coverage, current-state documentation, and a passing `e2e-tests` resource triggered through the Tilt MCP server.

---

## Remediation stage 0: Architecture migration plan

### Design and agreed policies

Use these policies and slices to implement the [architecture proposal](proposal.md) through the detailed tasks and verification gates in stages 1–4. The policies here resolve the alternatives still listed there; follow the selected paths during implementation.

- **Scope and upgrades:** target a shared internal pilot within one organisation, protecting credentials and isolating guests even when a guest is compromised. Use coordinated platform upgrades with brief downtime; preserve Application manifests, existing WIT binaries and stored data. Mixed-protocol rolling compatibility is deferred.
- **Artifacts:** share verified raw Wasm and compile locally on each host. Allow anonymous pulls from organisation-approved HTTPS registries, with an explicit local-development plaintext exception. Private registry authentication, shared native artifacts and compilation-versus-network benchmarking are deferred.
- **Identity and transport:** authenticate short-lived projected Kubernetes ServiceAccount tokens with a specific audience through TokenReview. Use Istio ambient strict mTLS and explicit service allowlists. One trusted shared execution-host pool may prepare every Application and hold its credentials; scoped guest capabilities enforce isolation.
- **Configuration authority:** only an initialized elected operator leader publishes configuration. Keep standby operators and webhook availability separate from publication authority. Loss of authoritative sync stops admission and cancels affected work; retained configuration does not provide a grace period.
- **Activation and retirement:** retain the working generation during ordinary replacement preparation and failure. Activate only the latest desired deployment once at least one healthy host is prepared; temporary reduced capacity is acceptable. Exclude unprepared hosts from traffic, drain accepted old work within its deadlines, then retire the old generation. Rollback deploys an earlier artifact as a fresh revision; there is no retained quick-rollback window.
- **Revocation:** removing or narrowing permission independently denies new forbidden operations and cancels affected in-flight work through backend cleanup. Unaffected work may continue; committed effects remain committed.
- **Data and ownership:** namespace/name identifies persistent Application data; current Application UID and policy identify authorization and ephemeral resource ownership. Retain data by default and reuse it on same-name recreation. Provision shared infrastructure on demand, reclaim existing resources only after verifying platform ownership and expected identity, and retain it until explicit administrator removal. Data purge also requires explicit administrator action.
- **Migrations:** use credentials scoped to the Application's logical database. Keep old traffic serving through running or failed migrations; developers must keep intermediate, successful and partially failed schema states compatible with serving code. Support both increasing and decreasing targets. Finish an active run before reconciling the newest desired artifact/target, skipping superseded pending requests; only the latest desired deployment may activate after exact migration and credential convergence. Opt-in traffic pauses are deferred.
- **ApplicationRevision:** persist immutable configuration snapshots, resolved artifact digests, metric definitions and resource references, with operator-owned rollout and cleanup status. Preserve exact effective inputs or recoverable versioned bindings; keep generated credentials in Secrets. Record revisions and participating host processes before publication. Retain cleanup obligations until actual preparation, invocation and collector ownership is released; unreachable participants remain unresolved until cleanup or verified termination/fencing. Protect deletion with finalization and bound unresolved revision state.
- **Metric schemas:** keep one metrics endpoint per host and preserve declared metric names. Compatible revisions may share collectors; an incompatible definition may reuse a name only after every conflicting revision has verifiably retired. Validate Application changes against desired configuration and surviving revision claims at admission; rejection preserves the previously accepted desired configuration. The elected operator rechecks and reserves ownership durably before publication to cover admission races. Fix host collector retirement so reuse works without restart; automatic schema suffixes are not selected.
- **Messaging:** require sender-declared destinations and receiver-declared permitted sources for cross-Application publication. Establish the Application origin in the trusted host and enforce current permissions at both boundaries. Protect internal dispatch and reply subjects. External producer onboarding is deferred; raw publishers cannot assert an Application origin.
- **Delivery and shutdown:** retain Core NATS live-event semantics, without durable replay or automatic handler/request retries. Stop intake, drain accepted work within a finite grace budget, then cancel remaining work and complete cleanup. Report rejected, dropped and unfinished outcomes. JetStream jobs and their retry/dead-letter policies are deferred.
- **Capacity:** share execution capacity fairly among active Applications, allow use of spare capacity, and reserve nothing for idle Applications. Functions share their Application's budget. Bound counts, bytes and time across work, dependencies and retained state; derive numeric limits from pod budgets and measurement.
- **Deployment:** the umbrella Helm release is the authoritative platform composition and compatible version set. Components retain build/test ownership; Istio, db-operator and other infrastructure prerequisites are managed separately. Reconcile conflicting documentation as implementation lands.

### Remaining implementation slices

Deliver each slice through its production entry points, add regressions for its changed external behavior, and pass the full existing `e2e-tests` resource through Tilt MCP. Run all builds, deployments and tests through Tilt MCP. Preserve the permanent fixtures and the additional fixtures already required by that resource; report validation evidence in the task conversation.

- [ ] **5 — Protected identity and startup authority.** Implement ServiceAccount authentication, Istio ambient transport protection, recipient authorization and rejection of uninitialized configuration.
- [ ] **6 — Application lifecycle and inventory.** Persist owned resources, safely transition legacy identities, and implement verified retention, recreation and reclamation.
- [ ] **7 — Effective capability authorization and revocation.** Enforce SQL/KV/metric isolation, current permission removal and mutual messaging grants. Release/pin and verify required downstream db-operator enforcement.
- [ ] **8 — ApplicationRevision foundation.** Add the immutable CRD, durable revision identity/configuration, participant and cleanup status, metric reservations, admission checks and protected retirement.
- [ ] **9 — Publication and replay.** Publish coherent configuration/routes and watermarks; implement restart epochs, bounded replay/resync, exact acknowledgements and recipient session ownership.
- [ ] **10 — Owned routes.** Enforce path ownership and apply atomic changes/tombstones for rename, removal, trigger conversion and deletion.
- [ ] **11 — Guest metric catalog.** Bind authorized schemas to generation leases, bound series, retire registrations and permit safe bare-name reuse without restart.
- [ ] **12 — Complete serving generations.** Prepare immutable modules, environment and capability bindings; fence supersession and failed preparation, with leases retained through accepted work.
- [ ] **13 — Distributed activation and recovery.** Activate with one healthy prepared host, fence gateway/NATS dispatch to the correct generation, exclude stale/unprepared hosts and recover from authority loss.
- [ ] **14 — Migration and credential convergence.** Verify exact desired migration outcomes, correct table-grant bootstrap ordering, isolate migration credentials, serialize target changes, support rollback and reconcile credential rotation.
- [ ] **15 — Capacity, fairness and deadlines.** Complete aggregate/per-Application bounds, fair scheduling, SQL connection/result limits, dependency deadlines and buffered HTTP response/header behavior.
- [ ] **16 — Finite shutdown and live-event delivery.** Bound drain/cancellation, acknowledgement and idle-session lifetimes; account for message loss and unfinished work without automatic retries.
- [ ] **17 — Release validation and documentation.** Complete reproducible umbrella deployment, generated-contract drift checks, CI and fresh-clone validation; reconcile current-state documentation with demonstrated behavior.

The corresponding stage 1–4 tasks and exit gates define detailed coverage. Add revision restart/retirement, mutual messaging permission, schema reuse, revocation and fair-admission checks in their owning slices; existing green tests alone do not establish those new guarantees.

---

## Remediation stage 1: Execution and trust containment

### Design

Resolve F01–F06 and the immediate exposure/resource paths in F09/F11/F12. Resource ownership must last until execution has actually exited. A timeout response alone is insufficient. Network restrictions are interim containment; they do not replace authentication or artifact provenance. Owners are the execution host, gateway, module cache, wp-operator, Helm configuration, and the downstream db-operator integration.

### Tasks

- [ ] **F03 — Protect raw-cache reachability.** Apply authenticated workload/service allowlists with the identity and transport slice so unauthorized clients cannot monopolize cache storage or intake.
- [ ] **F04 — Protect configuration credentials.** Authenticate host/gateway workload identities, authorize the configuration each identity may receive, encrypt snapshot/stream transport, and restrict network reachability. An arbitrary client-provided host ID must not authorize a full credential-bearing snapshot. Define certificate/identity issuance, rotation, expiry, and rejection behavior.
- [ ] **F05 — Enforce SQL policy throughout its lifecycle.** Preserve table selectors and every declared grant field, or reject unsupported scopes. Patch existing managed credential specs; enumerate owned credentials; revoke removed users, permission downgrades, SQL removal, and final deletion even after spec edits. Coordinate with pinned db-operator `v1.1.6`: its current grant-only updates and best-effort user-drop cleanup require repair or a verified enforcement alternative. Release/pin downstream changes and test the deployed version, rather than relying on a newer local checkout. Include sequence/default privileges and existing-session behavior in the effective policy; do not treat `Ready` or CR disappearance as proof of revocation.
- [ ] **F06 — Prevent credential/resource identity collisions.** Replace ambiguous concatenated names with an unambiguous encoded/hash tuple or owner-UID identity within Kubernetes name limits. Check owner UID and expected spec before adoption, Secret use, patching, or deletion. Define a safe transition for existing names without adopting another app's resource. Extend these ownership checks to fixed-name credentials and migration resources (F16).
- [ ] **F09 — Contain serving topology immediately.** Keep one configuration-serving operator replica and prevent overlapping rollout processes from serving private empty stores. Fail closed for uninitialized snapshots; complete initialization/leadership fencing in stage 2 before permitting multiple serving replicas. Single-replica settings alone do not fence startup or rollout overlap.
- [ ] **F11 — Close remaining immediate unbounded paths.** Set SQL result and query limits, Redis I/O deadlines, and per-app metric authorization/cardinality limits. Set a conservative aggregate invocation budget compatible with pod memory, including host allocations; complete capacity accounting in stage 3.
- [ ] **F18 — Begin the regression/CI gate now.** Add exact-revision build/unit checks and deterministic cancellation, race, ownership, and policy fixtures. Repair shared-source Tilt dependency inputs before relying on local regression results; expand the release gate in stage 4.

### Verification and exit gate

- [ ] Looping production components and delayed SQL/KV side effects stop within the declared cancellation bound; no pending delayed write occurs after expiry, and no new operation starts after cancellation. Account explicitly for an already-committed external operation. Actual active executions stay within the admission limit, including terminating work.
- [ ] Sustained successful traffic keeps task bookkeeping bounded and memory stable after warm-up; panic and cancellation paths release resources only after termination; shutdown completes within its budget.
- [ ] Anonymous/wrong-identity configuration and unauthorized cache requests fail; tampered or non-Wasm artifact bytes never become registered components; captured logs contain no secrets.
- [ ] Real database checks prove table isolation, narrowed table scopes, ALL-to-SELECT downgrade, sequence/default privilege limits, existing-session revocation, user removal, SQL removal, and deletion after edits. Failed downstream role deletion retains a retryable finalizer. Two apps using the `foo-bar`/`baz` and `foo`/`bar-baz` collision pair cannot share or delete each other's credentials; test both deletion orders.
- [ ] Oversized/slow input and dependency results terminate or reject within finite limits; cross-app metric use is denied. The e2e suite and new negative security tests pass.

---

## Remediation stage 2: Configuration and lifecycle convergence

### Design

Address F07–F10/F13 and the immutable-identity and migration portions of F12/F16. Separate desired, preparing, and serving state. Publish a prepared immutable application generation containing modules, SQL pools, capabilities, environment, metrics, subscriptions, and routes. Coordinate host and gateway activation so traffic cannot target an unprepared generation; define drain/retention behavior for the prior generation. Owners are wp-operator, execution host, gateway, protocol definitions, and db-operator integration.

### Tasks

- [ ] **F12 — Fence serving artifact identity at activation.** Bind persisted desired digests to participating host and gateway activation acknowledgements; record serving digests separately from desired module resolution. Never mutate a serving generation during automatic tag tracking.
- [ ] **F07 — Stage and atomically publish generations.** Fence module/pool insertion and eviction by Application UID and desired generation/revision. Late old loads, deleted functions, and delete/recreate under the same name cannot resurrect state. Prepare dependencies before activation and serving acknowledgments; distinguish received/preparing/applied/failed status. Retry transient preparation failures with bounded backoff while retaining the old working generation only under the documented compatibility policy. Revoke removed privileges promptly even when retaining old code; security downgrades must not wait indefinitely for unrelated preparation.
- [ ] **F08 — Make snapshot/watch delivery replayable.** Use one revision scheme and restart epoch for snapshots/deltas, an atomic snapshot-and-watch boundary or replayable revision log, ordered application, gap detection, and resync when replay is unavailable. Snapshot contents and watermark must be coherent. Registering first without ordering updates around the snapshot is insufficient. Cover hosts and gateways; clean stream closure must force recovery.
- [ ] **F09 — Fence configuration authority.** Serve snapshots/streams only from initialized authoritative state. Choose leader-only serving or coherent state on every serving replica; align readiness and Service selection with that authority, keeping webhook availability separate. Test startup, two replicas, leader transition, rolling replacement, and restart before relaxing the stage 1 topology restriction.
- [ ] **F10 — Own HTTP routes and remove them completely.** Persist owner identity, reject duplicate paths across apps/namespaces and within one app, reserve probe paths, and use owner-checked deletions. Keep admission and reconciliation checks consistent, including concurrent admissions. Apply atomic old/new route diffs with tombstones for rename, function removal, HTTP-to-topic conversion, and Application deletion; snapshot order must never select a conflicting winner.
- [ ] **F13 — Reconcile credential rotation and live readiness.** Watch NATS credential files and Kubernetes Secret/PostgresCredential changes, reload credentials used by reconnects, and compare full SQL pool configuration on both deltas and snapshots. Replace changed pools safely and remove obsolete users/pools. Derive readiness from real NATS/dependency connection, authoritative initialization, sync-stream state, and serving-generation preparation; clear it on disconnect or clean stream closure and restore it after recovery.
- [ ] **F16 — Fence migration and capability transitions.** Verify the exact deployed db-operator contract, including observed generation, target/current revision, and observed immutable artifact. Require readiness for the desired migration generation rather than accepting an old `Ready`. Order bootstrap so credentials scoped to migration-created tables cannot block creation of the migration that creates those tables. Define whether old traffic is drained or retained during pending/failed migrations and what schema compatibility is required. Enforce removal/revocation and ownership-safe finalization across update/remove/recreate, with explicit data-retention and downstream-deletion completion rules.

### Verification and exit gate

- [ ] Barrier-controlled tests reorder old/new loads and pool preparation, delete during load, delete/recreate, fail/recover dependencies, and send traffic through activation. Only the desired prepared generation serves; transient failures retry without lost messages silently masquerading as readiness.
- [ ] Create/update/delete in the snapshot-stream handoff gap, revision gaps, reconnects, and operator restarts converge without a manual restart. Slow-client resync and epoch changes cannot overwrite newer state.
- [ ] Multiple operators, startup before reconciliation, leadership change, and rolling replacement cannot publish an uninitialized empty authoritative snapshot or erase working state.
- [ ] Conflicting routes never cross owners. `/old` stops serving after rename to `/new`; removal, trigger conversion, and deletion remove routes without a gateway restart.
- [ ] NATS rotation, PostgreSQL rotation while disconnected, removed users after full resync, broker outage/recovery, and clean stream closure produce the correct readiness and credentials. Stale migration status, artifact/target changes during a running migration, and failed migrations on already-serving apps cannot activate an unready generation. Initial table-scoped credentials and their table-creating migration converge without deadlock. Tag movement cannot place digest B under digest A; replicas agree on one immutable generation.

---

## Remediation stage 3: Resource budgets and failure behavior

### Design

Complete F11–F17 after execution termination and generation convergence are dependable. Use finite budgets across all hops, including queueing, compilation, host calls, and shutdown. Document supported buffered HTTP and Core NATS semantics. Owners are execution host, gateway, module cache, wp-operator stream/session handling, shared runtime libraries, and Helm configuration.

### Tasks

- [ ] **F11 — Define and enforce total capacity.** Budget request/response headers and raw/serialized bodies, SQL rows/result bytes and pool acquisition, KV payloads/I/O, host-call concurrency, guest memory/table/resource counts, queues, and aggregate resident memory. Include serialization copies, compiled modules, metric series, and terminating invocations. Reconcile default concurrency and per-memory allowance with pod limits; budget SQL connections across host replicas × application users × pool sizes. Document placement/sharding as a scaling follow-up only if those budgets require it.
- [ ] **F11/F15 — Scope and reconcile metrics.** Authorize metric calls by owning Application, cap label size/series cardinality, retire removed definitions/series, and compare full type/label schemas. Implement safe schema updates or explicitly reject mutation at admission; updates must not silently depend on restart.
- [ ] **F12 — Complete preparation lifetime and memory accounting.** Integrate queued and active preparation into generation cancellation and shutdown. Retain accounting for replaced compiled components until accepted invocations release them. Budget compiler peak memory and serialization copies alongside registry storage; isolate or terminate over-budget compilation without prematurely releasing admission.
- [ ] **F14 — Align HTTP deadlines and header behavior.** Carry one end-to-end deadline through gateway, NATS, queueing, and execution; configure broker request timing consistently and distinguish timeout (504), unavailability, and malformed responses. Preserve valid repeated headers such as `Set-Cookie`, while filtering unsafe framing/hop-by-hop headers. Keep streaming/WebSocket support explicitly outside the current buffered contract.
- [ ] **F15 — Honor declared guest environment.** Populate each invocation's WASI context from declared Application environment in both HTTP and message worlds. Do not inherit host environment or leak values across applications.
- [ ] **F16 — Validate identifiers and lifecycle inputs.** Enforce unique function/user/metric names, safe resource and NATS identifiers, reserved prefixes/paths, and supported grant scopes at admission and reconciliation. Test already-running app failures, ownership mismatches, capability removal, and finalizer retry behavior rather than create-only success.
- [ ] **F17 — Bound shutdown and stream/session lifetimes.** Stop intake, drain accepted queued and active work under one finite grace budget, then cancel/terminate remaining work under stage 1's contract. Add gateway graceful shutdown and bounded operator stop. Make idle stream waits/ACKs cancellation-aware with deadlines; use registration session tokens so old cleanup cannot remove a replacement session. Specify counts/status for rejected, dropped, and unfinished work.
- [ ] **F17 — Decide the event-delivery contract before topic expansion.** Document Core NATS as best-effort live events if that meets the product requirement. If durable jobs are required, add JetStream acknowledgment, retry, deduplication/idempotency, and dead-letter semantics as prerequisite work. Provisioned JetStream storage alone supplies none of those guarantees; do not promise exactly-once side effects.
- [ ] **F11–F17 — Complete operational coverage.** Extend lifecycle metrics across components; expose preparation/retry state, serving revision/digests, pool connection counts, metric cardinality, and cache occupancy. Define alerts from the documented budgets, readiness transitions, and recorded lifecycle history.

### Verification and exit gate

- [ ] Slow/oversized bodies, serialized responses, large SQL results, blocked Redis/SQL, label growth, and cold-start bursts reject or finish within bounds without unbounded process growth or exceeding documented connection budgets.
- [ ] Tag races, cache GET/PUT stalls, supported large artifacts, malformed artifacts, and cache eviction cannot strand activation or admit unverified code.
- [ ] An 11-second reply succeeds under a 30-second configured deadline; real expiry returns 504; multiple cookies survive the round trip; unsafe headers are rejected. Both invocation worlds receive only their own declared environment, and metric schema changes follow the explicit policy without a restart.
- [ ] Filled queues at SIGTERM, looping guests, idle disconnects, missing ACKs, and overlapping reconnects exit within the declared grace period. Accepted messages follow the chosen delivery policy; readiness reflects outage/recovery and every dropped/unfinished item is accounted for.

---

## Remediation stage 4: Reproducible development and releases

### Tasks

- [ ] **F18 — Establish canonical schema generation.** Establish one canonical CRD generation/deployment path and a generated-source/schema drift check; do not hand-edit generated outputs.
- [ ] **F18 — Complete release validation.** Extend the local Tilt CI gate with reproducible generators and schema/protobuf drift checks. Align Makefile commands and remove misleading envtest/integration claims. Pin tool/dependency versions and verify release artifacts through the same real-cluster tests.
- [ ] **F01–F18 — Make failure tests release gates.** Include the cases above in automated regression coverage, prioritizing F01–F13 and F17. Cover load/soak, update/reconnect/rollout, secret redaction, SQL/metric isolation, migration status, and shutdown. Do not substitute line-coverage percentages for verified failure contracts.
- [ ] **Documentation — Reconcile claims with implementation.** Update the owning README/architecture/contribution/component docs as fixes land. Distinguish implemented guarantees, development-only defaults, planned features, and measured results. Correct credential rotation, gateway settings, SQL/Redis topology and connections, topic publish policy, admission, deletion/retention, migration rollback, and wire contracts; link to source schemas rather than duplicate them. Mark CLI commands unimplemented while the component is only a README. Remove unsupported cold-start/concurrency/pooling/mmap/HPA/KEDA claims from current-state docs; keep goals here until measured or implemented.
- [ ] **Onboarding — Prove a fresh clone.** Provide one tested prerequisites/build/apply/wait/curl/expected-result/cleanup path for HTTP and SQL, including KUBECONFIG export, actual standalone guest target paths, pushed module and migration artifacts, troubleshooting, development security boundaries, and data retention. Align guest/component contribution guidance with the current workspace/umbrella chart. Resolve an explicit license before outside adoption.
- [ ] **SQL pooling — Restore warm connection reuse.** Return healthy execution-host PostgreSQL connections to the pool after confirmed COMMIT/ROLLBACK and bounded session-state cleanup, so function invocations benefit from warm connections. Close cancelled, terminated, or uncertain connections and retain admission permits through cleanup. Verify backend reuse across successful calls, transaction/session isolation, and cancellation safety; measure connection churn and warm SQL latency.
- [ ] **Performance and operations — Measure after correctness gates.** Define target workloads and deployment sizes, then measure warm/cold latency distributions, throughput, aggregate memory, SQL connection fanout, and outage recovery. Test backup/restore and upgrade/rollback compatibility; prepare budget/readiness alerts, incident and rotation runbooks, and dependency/security maintenance before claiming production readiness.

### Verification and exit gate

- [ ] A clean clone completes the documented HTTP/SQL path with no hidden steps; shared edits rebuild consumers; regeneration is reproducible with no deployed-schema drift.
- [ ] The release pipeline runs the production invocation path and required negative/race/transition tests against the release artifacts; generated artifacts, Helm validation, tagged integration, and `e2e-tests` all pass.
- [ ] Current-state documentation matches demonstrated behavior. Performance claims include workload, hardware, configuration, and measured distributions; readiness claims meet the gates above.

---

## Phase 13: Middlewares

Deferred until remediation stages 1–4 pass. Middleware must use the authenticated artifact path, immutable generation activation, owner-scoped capabilities, and one cancellation/resource budget for the entire request/response chain. Explicitly authorize global publication and cross-Application use; a shared compiled component must not share invocation secrets, context, or permissions.

### Design

Add composable middleware support for HTTP functions. Middlewares are full WIT components that the execution host loads, AOT-compiles, and chains in-process — no extra network hops. HTTP-only for the initial implementation.

A function with `trigger.middleware` (and `type: http`) is a middleware module rather than a handler. Middlewares can be scoped to their own Application (default) or declared `global: true`, making them referenceable by name from any Application's middleware chain. The operator enforces cluster-wide uniqueness for global middleware names. An app-local middleware with the same name as a global middleware shadows the global one within that application — local always wins. Middlewares are composed at two levels: top-level `spec.middlewares[]` applies to all HTTP functions in the Application; `spec.functions[].middlewares[]` applies to a specific function. The effective chain for a request is `spec.middlewares` + `spec.functions[].middlewares` + handler, in order. `on-response` is called in reverse order after the handler returns.

**WIT contract — `http-middleware` world (new):**

The `http-types` interface gains two new fields on `http-request` (`cookies: list<string>` and `context: list<u8>`), and a new `middleware-abort` variant:

```wit
variant middleware-abort {
    response(http-response),  // early bailout — return this response directly
    error(string),            // abort with a 5XX
}
```

A middleware exports two functions:
- `on-request(request: http-request, response: http-response) -> result<tuple<http-request, http-response, list<u8>>, middleware-abort>` — happy path returns modified request, modified response, and an opaque per-middleware context blob. Error aborts the chain.
- `on-response(request: http-request, response: http-response, context: list<u8>) -> result<http-response, string>` — receives the original request, the accumulated response from the handler and later middlewares, and the context blob stashed from `on-request`. Returns a modified response or an error to generate a 5XX.

**Breaking WIT change:** `http-application.on-request` changes to `on-request(request: http-request, response: http-response) -> result<http-response, string>` — the handler receives the middleware-accumulated response as input. All existing `http-application` guest modules must be recompiled against the new WIT. This is a **breaking change for all existing HTTP modules**.

The `http-request.context` field is the shared, mutable state visible across the whole chain (e.g. an injected user ID written by auth middleware and read by the handler). The per-middleware `context: list<u8>` returned from `on-request` is the middleware's private stash, passed back only to its own `on-response`.

### Tasks

- [ ] **Breaking WIT change:** update `http-types` — add `cookies: list<string>` and `context: list<u8>` to `http-request`; add `middleware-abort` variant; define the new `http-middleware` world; update `http-application.on-request` to accept a pre-configured `http-response` argument. Document blast radius: all `http-application` guest modules require recompilation.
- [ ] Update the execution host to chain middleware modules for HTTP invocations: call each middleware's `on-request` in order, passing accumulated request and response; on abort variant, return the response or generate a 5XX immediately; call each middleware's `on-response` in reverse order after the handler returns.
- [ ] Add `trigger.middleware` to `Application` CRD `FunctionSpec`: fields `type` (enum, only `"http"` for now) and optional `global` (bool, default false).
- [ ] Add `spec.middlewares[]` (ordered list of middleware names, applied to all HTTP functions) and `spec.functions[].middlewares[]` (ordered list of middleware names for a specific HTTP function) to the `Application` CRD.
- [ ] Operator: enforce cluster-wide uniqueness for global middleware names (`MiddlewareConflict` status condition, covered by the ValidatingWebhook once in place).
- [ ] Add a `ModuleKey` message to `configsync.proto` carrying `namespace`, `app_name`, `fn_name`, and owning Application UID, plus a `module_key` field on `FunctionConfig`. The operator supplies the canonical owner: a regular/local function's own app or the declaring app for global middleware. Resolve app-local names before global names. Version `ModuleRegistry` entries by owner identity plus resolved immutable module digest and engine compatibility identity; share `Arc<Component>` only for the same immutable version. Keep versions referenced by serving/retained/in-flight generations until drained, and make eviction reference-aware. Restrict any consumer-identity fallback to legacy regular/local functions; reject missing owner identity for global middleware.
- [ ] Operator: include middleware module refs, their resolved `ModuleKey`s, and chain order in the config pushed to execution hosts via ConfigSync; execution hosts load and AOT-cache middleware modules through the existing module-cache path.
- [ ] Define middleware-owner versus consuming-Application authority for SQL, KV, environment, messaging, and metrics; authorize global publication/use explicitly. Sharing a compiled `Component` must keep invocation Stores, secrets, context, and capabilities isolated. Reject missing/invalid owner identity for global middleware rather than falling back to consumer credentials.
- [ ] Bound chain length and shared/private context bytes; use one deadline and aggregate resource budget across all `on-request`, handler, and `on-response` calls. Define unwind behavior for abort, trap, and cancellation; stage the entire chain in one immutable generation.
- [ ] Update all existing `http-application` guest modules in `examples/` to match the new `on-request` signature.
- [ ] Add e2e test: deploy a global auth middleware and an HTTP function with it in its chain; verify that a request without a valid auth token gets a 401 from the middleware (chain aborted), and a valid request reaches the handler.
- [ ] Add regressions for cross-app secret/capability denial, shared-module state isolation, a looping middleware, oversized context/chain, and update/delete during chain preparation. Verify handler side effects do not start after cancellation.
- [ ] Update `framework/runtime.wit` documentation comments; update the project README and `docs/architecture.md` to document the middleware execution model and the two-phase (`on-request` / `on-response`) contract.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. The suite must pass with tests confirming: (1) a global auth middleware rejects requests without a valid token with a 401 before the handler is reached; (2) a valid request passes through the middleware chain and the handler returns the expected response; (3) a middleware using `on-response` can modify the handler's response before it is returned to the caller; (4) all existing HTTP function e2e tests continue to pass after recompilation against the new WIT.

---

## Phase 14: Static file serving

Deferred until remediation stages 1–4 pass. Reuse authenticated, revision-safe configuration, immutable OCI identity, finite artifact budgets, and ownership-safe route activation/deletion.

### Design

Add a `trigger.static` function trigger type to the `Application` CRD, enabling frontend static assets to be served from an ORAS artifact reference. This allows the full vertical slice of an application — frontend and backend — to be declared in a single `Application` CRD.

wp-operator provisions a new `static-host` Rust component (under `components/`) that connects to wp-operator via a dedicated `StaticSync` gRPC service with the repaired ConfigSync authentication, replay, and authority guarantees. On receiving a config push, the static host fetches the ORAS artifact by verified immutable digest over HTTPS (plaintext only for an explicitly allowlisted local development registry), safely unpacks it to a versioned on-disk directory, and activates the prepared generation with the gateway route. There is no module-cache intermediary and no compile step — each static-host replica maintains its own bounded on-disk cache. The gateway routes static-path requests to the static-host Service.

Static paths are subject to the same cluster-wide uniqueness enforcement as HTTP trigger paths (`StaticPathConflict` condition; included in the validating webhook scope).

### Tasks

- [ ] Add `trigger.static` to the `Application` CRD `FunctionSpec`: `path` (URL path prefix, unique cluster-wide) and `artifact` (ORAS artifact reference to the static bundle).
- [ ] Design and implement the `StaticSync` gRPC proto: `RequestFullStaticConfig` and `PushStaticUpdate` carrying owned paths, immutable artifact identities, and generation/revision metadata; reuse authenticated replay/gap recovery from the repaired `ConfigSync` protocol.
- [ ] Create `components/static-host/`: a Rust binary with a `StaticSync` gRPC client, a secure ORAS pull client, and an async HTTP file server; stage files before generation-fenced serve-root/route activation.
- [ ] Enforce download/extraction/file-count/expanded-size/disk-cache budgets and deadlines; reject archive traversal, unsafe symlinks, and requests escaping the serve root. Define overlapping static/HTTP prefix ownership and reserved paths; evict unused generations safely after draining.
- [ ] wp-operator: track connected static-host replicas; push incremental updates on Application create/update/delete with static functions; serve a full snapshot on static-host startup.
- [ ] Gateway: add static-path route entries pointing to the static-host Service; pass-through proxy for matched paths.
- [ ] Helm chart: add `static-host` Deployment and Service; add static-host gRPC address to wp-operator config.
- [ ] Add `StaticPathConflict` status condition to `Application` (parallel to `TopicConflict`) and include static path uniqueness in the validating webhook scope.
- [ ] Update wp-operator README: document `trigger.static` fields, static-host behaviour, and path uniqueness rules.
- [ ] Add e2e test: deploy an `Application` with both a `trigger.http` WASM function and a `trigger.static` function; verify assets are served at the static path and the WASM function responds on its HTTP path.
- [ ] Add negative/update tests for traversal and unsafe archives, oversized bundles, untrusted config/artifacts, overlapping paths, out-of-order preparations, and rename/delete without restart; replicas must serve the same digest/generation.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. The suite must pass with tests confirming: (1) static assets declared in `trigger.static` are served at the configured path prefix; (2) WASM HTTP functions in the same Application continue to respond correctly; (3) applying an `Application` with a static path that conflicts with an existing one is rejected at admission; (4) deleting the Application removes the static routes from the gateway.

---

## Phase 15: Multi-Subscriber Topics

Deferred until remediation stages 1–4 pass and the stage 3 event-delivery contract is explicit. Cross-Application subscriptions/publication require authorization consistent with the tenant threat model.

### Design

Permit multiple authorized functions across Applications to subscribe to the same event topic. Derive each function's consumer-group identity from an unambiguous encoded/hash tuple of its owning Application identity and function name, rather than the topic name or ambiguous hyphen concatenation. Replicas of one subscriber share a group; distinct subscribers receive independent deliveries under the chosen event-delivery policy.

Before enabling shared call-response topics, define explicit responder and observe-only roles. The proposed baseline is one authorized responder per request topic, with observers unable to reply; validate this across execution-host replicas. A host-local first-reply rule cannot arbitrate independent hosts or prevent duplicate side effects. Specify observer side effects, reply timeouts, retries, and idempotency under the chosen delivery policy before removing responder ownership checks.

Remove blanket `TopicConflict` checks only for authorized shared event subscriptions. Retain or replace conflict checks for responder ownership and invalid subscription roles.

### Tasks

- [ ] Implement collision-resistant subscriber-group identities and subscription/publication authorization; remove event-topic uniqueness only where the delivery policy permits sharing.
- [ ] Define responder/observer roles in the contract and admission/status validation. Enforce one authorized responder across the platform and prevent observers from publishing to request reply subjects.
- [ ] Update e2e tests across multiple host replicas: two authorized event subscribers each receive the published message; only the designated responder can produce a request reply, while observers receive their permitted copy and cannot reply; ambiguous-name tuples, unauthorized subscriptions, reconnects, and shutdown follow the declared policy.
- [ ] Update wp-operator README and `docs/architecture.md` to document multi-subscriber semantics and consumer group identity.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. The suite must pass with tests confirming: (1) two authorized Applications subscribed to the same event topic both receive a published message; (2) a shared call-response topic has one authorized responder and observers cannot reply, including across replicas; (3) authorized event sharing is admitted while conflicting responder ownership and unauthorized topic access are rejected; (4) side effects, retries, and drain behavior match the chosen delivery policy.

---

## Phase 16: Web UI

Deferred until remediation stages 1–4 pass. The UI must expose trustworthy desired/preparing/serving state and authenticate users with namespace/Application authorization before shared use.

### Design

Add a browser-based UI that gives platform operators and developers a live view of the running platform. The UI is served as a single-page application by a new `wp-ui` component, backed by a lightweight Go API server that aggregates data from the Kubernetes API, Loki, and Prometheus.

**Graph view (default):** renders only Applications and relationships authorized for the current user as a dependency graph — functions and middlewares are nodes; middleware chain membership and topic subscriber relationships are directed edges. Each node is colour-coded by readiness (`Ready: True` → green, degraded → amber, unknown/error → red). Hovering a node shows a tooltip with the function name, trigger type, and active feature flags (KV, SQL). This is the landing page.

**Function / middleware detail panel:** clicking any node opens a right-hand panel with three tabs:

- **Status:** desired/preparing/serving generation, current `Ready` and other conditions (e.g. `MetricConflict`, infrastructure state), desired and serving immutable OCI digests required by remediation stage 2, and module size from the prepared artifact metadata.
- **Logs:** a streaming Loki log view scoped to the selected function (`{namespace, app, function}` label set), expressed as a LogQL query. The UI sends the query to the `wp-ui` API server, which proxies it to the configured Loki HTTP endpoint.
- **Metrics:** a PromQL-backed sparkline and table for the function's invocation count, error rate, and p99 latency. The UI sends the query to the `wp-ui` API server, which proxies it to the configured Prometheus HTTP endpoint.

**Architecture:**
- `components/wp-ui/` — a Go binary with two responsibilities: (1) serve the compiled SPA from an embedded `embed.FS`; (2) expose an authenticated JSON REST API (`/api/applications`, `/api/logs`, `/api/metrics`) that the SPA calls. Give its in-cluster Kubernetes client only the required read-only RBAC, and independently authorize each user's namespace/Application access. Construct scoped Loki/Prometheus queries server-side and keep credentials out of browser responses.
- The SPA is built from a `web/` directory inside `components/wp-ui/` using a standard Node.js build step (Vite or similar). The build output is embedded at compile time via `go:embed`.
- Helm chart: add `wp-ui` Deployment, Service, and a read-only `ClusterRole` / `ClusterRoleBinding` for `applications` resources. Loki and Prometheus endpoints are configurable via Helm values.

### Tasks

- [ ] Scaffold `components/wp-ui/`: Go module, `main.go`, `web/` SPA skeleton (Vite + TypeScript), Dockerfile, and Tiltfile resource.
- [ ] Implement user authentication and namespace/Application authorization; restrict Kubernetes reads and telemetry proxy access, redact secret-bearing fields, and bound query ranges, results, concurrency, and rates.
- [ ] Implement the `/api/applications` endpoint: list only authorized `Application` CRDs, returning name, namespace, desired/serving generation, preparation/readiness conditions, immutable module identities, trigger types, and feature flags (`spec.sql`, `spec.kv`).
- [ ] Implement the graph view SPA page: render functions and middlewares as nodes, middleware chain and topic edges as directed edges; colour-code by readiness.
- [ ] Show status and feature flags (KV, SQL) as node annotations in the graph.
- [ ] Implement the detail panel — Status tab: display desired/preparing/serving state, all readiness/status conditions, and the desired and serving immutable artifact metadata.
- [ ] Implement the `/api/logs` proxy endpoint: authorize `namespace`, `app`, and `function`, validate/escape labels and bounded time-range parameters, construct a scoped LogQL query, and stream bounded NDJSON responses with dependency deadlines.
- [ ] Implement the Logs tab in the detail panel: call `/api/logs` and render a live-scrolling log view.
- [ ] Implement the `/api/metrics` proxy endpoint: accept authorized function/metric selections and bounded time ranges; construct allowlisted scoped PromQL server-side and return bounded series data with finite dependency deadlines.
- [ ] Implement the Metrics tab: display invocation count, error rate, and p99 latency sparklines and summary table via `/api/metrics`.
- [ ] Add `wp-ui` Deployment, Service, `ClusterRole`, and `ClusterRoleBinding` to the Helm chart; add `wpUi.lokiEndpoint` and `wpUi.prometheusEndpoint` Helm values.
- [ ] Update `docs/architecture.md` to document the `wp-ui` component, its data sources, and the server-side proxy pattern.
- [ ] Add e2e test: deploy an Application, assert that `/api/applications` returns it with correct status; assert that the UI root (`/`) returns HTTP 200.
- [ ] Add negative e2e tests for unauthenticated/cross-namespace access, secret exposure, injected labels/queries, oversized ranges/results, and slow telemetry services; verify desired and serving state during failed activation/recovery.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. The suite must pass with tests confirming: (1) the `wp-ui` service returns HTTP 200 for the SPA root; (2) `/api/applications` returns only authorized Applications with correct status/generation and feature flags; (3) scoped Loki/Prometheus proxies work within budgets and deny unauthorized access; (4) negative security tests and all existing e2e tests pass.

---

## Future Work: Automated OCI Tag Tracking

Immutable digest resolution, digest-only fetches, status recording, and replica convergence are required by remediation stage 2 (F12). After that gate, optional automatic tracking of upstream tags may trigger a new desired generation under an explicit rollout policy. Never change the digest of an already-serving generation.

### Tasks

- [ ] Define opt-in tag-tracking cadence/webhook authentication and approval/rollback behavior for upstream changes.
- [ ] Re-resolve tags under bounded registry access, then prepare and activate a new immutable generation through the repaired convergence protocol.
- [ ] Test tag movement during polling, failed preparation, and rollback to a prior known digest without mutating serving-generation identity.

---

## Future Work: Distributed Tracing (OpenTelemetry)

Add request-scoped trace propagation across component boundaries (gateway → NATS → execution host → host functions) so that a single user request can be traced end-to-end.

### Tasks

- [ ] Integrate `opentelemetry` + `tracing-opentelemetry` in Rust components; propagate trace context through NATS headers.
- [ ] Add OpenTelemetry exporter configuration (OTLP endpoint, sampling rate) as env vars.
- [ ] Inject trace/span IDs into structured log entries for log–trace correlation.

---

## Future Work: Circuit Breakers

Add circuit-breaker logic to outbound dependency calls (module cache, database pools, NATS) so that sustained failures trigger fast-fail rather than timeout accumulation.

### Tasks

- [ ] Evaluate circuit-breaker crate options (e.g. `again`, `backon`, or a thin custom wrapper).
- [ ] Apply circuit breakers to module-cache HTTP calls and database pool acquisition.
- [ ] Surface circuit state (closed/open/half-open) as a Prometheus metric.

---

## Future Work: Request-Scoped Correlation IDs

Assign a unique correlation ID to each inbound request at the gateway and propagate it through NATS headers and log entries so that all log lines for a single request can be aggregated.

### Tasks

- [ ] Generate a correlation ID at the gateway (UUID or similar) and attach it to the NATS message headers.
- [ ] Extract and attach the correlation ID as a `tracing` span field in the execution host.
- [ ] Include the correlation ID in guest log forwarding so application logs are correlated with platform logs.
