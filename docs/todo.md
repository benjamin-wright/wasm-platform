# TODO

Active implementation plan for the wasm-platform project.

Use the [architecture proposal](proposal.md) for target responsibility boundaries and ownership invariants while executing this plan. Task order and remediation gates remain here.

Finding IDs refer to the supplied Wasm platform technical review. F01–F11 are P1; F12 combines P1 correctness with P2 resilience; F13–F18 are P2. These are engineering priorities.

## Delivery order and readiness gates

Retain Rust/Wasmtime, the Go operator, WIT capability boundaries and verified OCI distribution. Adopt immutable application revisions, assigned execution pools, direct HTTP dispatch and bounded serving-authority leases. The next milestone is reliable execution and isolation under updates, failures, and load. Deliver remediation stages 1–4 before resuming Phases 13–16; the shared runtime and local `wp run` / `wp test` workflow are part of this foundation. Pair each change with its regression coverage and use the [local Tilt CI gate](contributions.md#local-ci).

| Order | Milestone | Required outcome |
|---|---|---|
| 0 | Apply the architecture migration plan | Compatibility requirements and dependency-ordered slices guide delivery of stages 1–4. |
| 1 | Contain execution and trust risks | Timed-out work stops; task records are reaped; credentials, compiled artifacts, and SQL policy have enforceable boundaries. |
| 2 | Make revisions and placement converge | Prepared revisions, scoped pool assignments, replayable publication, authority leases, data bindings and migration fencing survive updates and reconnects. |
| 3 | Separate HTTP and event execution | Direct HTTP, versioned guest interfaces, finite budgets and explicit event/drain semantics. |
| 4 | Verify the development and release path | Shared-runtime local CLI, reproducible CI, failure-mode regressions, fresh-clone examples and accurate documentation. |

- **Trusted local prototype:** restrict authorship and service reachability to trusted participants. Verify fresh builds and tests; fix execution/task lifetime, stale build inputs, and setup instructions before calling it operational.
- **Shared internal pilot:** require every P1 repair, stages 1–4, assigned pools with scoped credentials, tested rotation and authority-lease expiry, dependency outage/restart recovery, migration and event-delivery policies, and a working CI gate. Keep publication single-authority until stage 2 proves otherwise.
- **External or mutually untrusted users:** additionally require a documented tenant threat model and validated process/pool isolation, workload identity, quotas, secure registry policy and negative security tests. Do not treat logical capability checks in a shared process as sufficient evidence for every tenant threat model.
- **Production:** additionally require workload-specific load/soak/failover tests, alerts and runbooks, backup/restore tests, release compatibility, upgrade/rollback procedures, and dependency/security maintenance.

### Verification

Use deterministic clocks, controllable loaders, and barriers for ordering tests; use a disposable cluster for database, credential, routing, and rollout behavior. Tests must exercise the production component/dispatch path and use finite client deadlines. Primitive fuel/memory tests or successful CR updates alone do not establish the platform guarantees. Report results in the task conversation, following [AGENTS.md](../AGENTS.md).

Keep existing `demo-app`, `counter-app`, and `sql-hello` fixtures passing. Follow the definition of done in [standards.md](standards.md): changed external behavior requires e2e coverage, current-state documentation, and a passing `e2e-tests` resource triggered through the Tilt MCP server.

---

## Remediation stage 0: Architecture migration plan

### Design and agreed policies

Use the [architecture proposal](proposal.md) for target contracts and boundaries. These delivery policies and slices replace the earlier all-host broadcast, HTTP-over-NATS, mandatory shared-cache and immediate-stop-on-sync-loss assumptions. Proposed behavior is not an implemented guarantee.

- **Scope and upgrades:** target a shared internal pilot within one organisation. Use coordinated platform upgrades with brief downtime; preserve Application manifests, existing guest binaries and stored data through explicit adapters and ownership-verified migration. Version new guest contracts rather than modifying the legacy world in place. Mixed-control-protocol rolling compatibility is deferred.
- **Artifacts:** verify immutable OCI identities and compile locally. Deduplicate compiled components by Wasm digest and engine compatibility identity, keeping application bindings and invocation state separate. Make the shared raw-Wasm cache optional; measure registry traffic and compilation before adding shared infrastructure. Retain approved HTTPS origins and the explicit local-development plaintext exception. Private registry authentication and shared native artifacts remain deferred.
- **Identity and placement:** authenticate short-lived projected ServiceAccount tokens with a specific audience through TokenReview; retain ambient mTLS and explicit allowlists. Assign applications to bounded tenant/trust-cohort pools with distinct workload identities. Publish only assigned revisions and credentials. Start with deterministic placement and Kubernetes Deployments/Services; a general-purpose scheduler is out of scope.
- **Configuration authority:** only an initialized elected leader publishes revisions and renews serving-authority leases. Keep webhook availability independent. A transient sync interruption permits the last committed revision to serve only until its existing lease expires; reconnect and replay cannot extend authority. Explicit revocation stops affected work immediately on receipt. Select and test lease duration, renewal cadence, clock handling and maximum revocation delay before enabling this behavior.
- **Activation and retirement:** retain the working generation during ordinary replacement preparation and failure. Activate only the latest desired deployment once at least one healthy host is prepared; temporary reduced capacity is acceptable. Exclude unprepared hosts from traffic, drain accepted old work within its deadlines, then retire the old generation. Rollback deploys an earlier artifact as a fresh revision; there is no retained quick-rollback window.
- **Revocation:** removing or narrowing permission independently denies new forbidden operations and cancels affected in-flight work through backend cleanup. Unaffected work may continue; committed effects remain committed.
- **Data and ownership:** explicit durable data resources identify retained data independently of Application deployment identity. Application UID and policy identify access and ephemeral ownership. Preserve existing namespace/name-derived data through ownership-verified legacy bindings; do not silently create replacement databases or make new explicit bindings depend on name reuse. Retain data by default; rebind, adopt and purge only through explicit authorized operations. Keep independent ResourceInventory cleanup obligations.
- **Migrations:** use credentials scoped to the Application's logical database. Keep old traffic serving through running or failed migrations; developers must keep intermediate, successful and partially failed schema states compatible with serving code. Support both increasing and decreasing targets. Finish an active run before reconciling the newest desired artifact/target, skipping superseded pending requests; only the latest desired deployment may activate after exact migration and credential convergence. Opt-in traffic pauses are deferred.
- **ApplicationRevision:** persist immutable effective configuration, resolved digests, guest API versions, data/capability bindings and metric definitions. Record pool assignments separately with durable epochs, and track desired, prepared, serving and retired state with exact participant acknowledgements. Generate routes and host projections from one durable publication sequence. Keep generated credentials in versioned Secrets and preserve cleanup obligations through finalization until work is released or termination/fencing is verified.
- **Metric schemas:** scope instrument ownership by tenant/Application while retaining declared names where compatible. Decide how incompatible same-name schemas are represented at scrape/export before allowing them in a shared pool; do not rely on labels to resolve Prometheus type conflicts. Reserve schema ownership durably, bound label values and active series, and retire collectors after their last generation lease. Cluster-wide user naming is not the target API.
- **Messaging:** require sender-declared destinations and receiver-declared permitted sources for cross-Application publication. Establish the Application origin in the trusted host and enforce current permissions at both boundaries. Protect internal dispatch and reply subjects. External producer onboarding is deferred; raw publishers cannot assert an Application origin.
- **HTTP and guest API:** move synchronous requests to an authenticated direct HTTP/2 or gRPC transport targeting prepared revision endpoints. Carry invocation identity, revision, deadline/cancellation and trace context. Choose the wire transport before implementation and define retry behavior; do not automatically retry non-idempotent requests. Adopt versioned WASI HTTP for new HTTP guests with a bounded legacy adapter, and version custom capability interfaces with typed errors and explicit result limits.
- **Delivery and shutdown:** NATS carries authorized live events, without durable replay or automatic handler retries. Stop intake, drain accepted work within a finite grace budget, then cancel remaining work and complete cleanup. Report rejected, dropped and unfinished outcomes. Durable jobs require a separate API and JetStream acknowledgment, retry, idempotency and dead-letter policies before implementation.
- **Capacity:** share execution capacity fairly among active Applications, allow use of spare capacity, and reserve nothing for idle Applications. Functions share their Application's budget. Bound counts, bytes and time across work, dependencies and retained state; derive numeric limits from pod budgets and measurement.
- **Deployment:** the umbrella Helm release is the authoritative platform composition and compatible version set. Components retain build/test ownership; Istio, db-operator and other infrastructure prerequisites are managed separately. Reconcile conflicting documentation as implementation lands.
- **Local development:** extract one runtime library used by execution hosts and cluster-free `wp run` / `wp test`. Serve HTTP on localhost and use in-memory KV/live-event adapters. For SQL, accept an existing PostgreSQL URL or lazily manage a project-scoped Docker PostgreSQL instance with persistent development data and explicit reset. Docker is unnecessary without managed SQL. Keep the same capability/cancellation code and real-cluster coverage for placement, identity, ownership and rollout.

### Remaining implementation slices

Deliver each slice through its production entry points, add regressions for its changed external behavior, and pass the full existing `e2e-tests` resource through Tilt MCP. Run all builds, deployments and tests through Tilt MCP. Preserve the permanent fixtures and the additional fixtures already required by that resource; report validation evidence in the task conversation.

- [ ] **7 — Effective capability authorization and revocation.** Enforce SQL/KV/metric isolation, current permission removal and mutual messaging grants. Release/pin and verify required downstream db-operator enforcement.
- [ ] **8 — ApplicationRevision foundation.** Add immutable effective inputs, API versions, data/capability references, separate durable assignments, desired/serving status, participant acknowledgements, schema reservations and protected retirement.
- [ ] **9 — Publication, replay and authority leases.** Publish coherent revision/route projections and watermarks; implement epochs, bounded replay/resync, exact acknowledgements and recipient session ownership. Define expiry/renewal, revocation latency and leadership fencing before allowing disconnected serving.
- [ ] **10 — Owned routes.** Enforce path ownership and apply atomic changes/tombstones for rename, removal, trigger conversion and deletion.
- [ ] **11 — Guest metric catalog.** Scope ownership, settle shared-pool export/schema conflicts, bind schemas to generation leases, bound series and retire registrations without restart.
- [ ] **12 — Complete serving generations and local code cache.** Prepare immutable modules, environment and capabilities; deduplicate compilation by digest/engine identity, separate bindings, make remote raw caching optional and retain code through accepted work.
- [ ] **13 — Pool placement and distributed activation.** Assign bounded application sets and credentials to authenticated pools. Activate with a healthy prepared endpoint, fence HTTP/event dispatch to the correct revision and test reassignment, stale participants and lease expiry.
- [ ] **14 — Durable data, migrations and credentials.** Add explicit data resources/bindings and ownership-verified legacy adoption. Verify exact migration outcomes, bootstrap ordering, isolated credentials, serialized target changes, rollback and rotation.
- [ ] **15 — Direct HTTP and versioned guest APIs.** Select the direct transport; implement prepared-endpoint discovery, cancellation/deadlines and invocation/trace identity. Add WASI HTTP and typed bounded capability interfaces with legacy adapters and compatibility fixtures.
- [ ] **16 — Capacity, finite shutdown and live events.** Complete fair admission and aggregate/per-Application bounds, SQL connection/results, bounded HTTP streaming, dependency and session deadlines, drain/cancellation and honest event-loss accounting.
- [ ] **17 — Shared runtime, local CLI and release validation.** Extract runtime/capability/lifecycle code early as other slices need it; deliver cluster-free `wp run` / `wp test`, reproducible deployment, generated-contract drift checks and fresh-clone validation.

The corresponding stage 1–4 tasks and exit gates define detailed coverage. Add revision restart/retirement, mutual messaging permission, schema reuse, revocation and fair-admission checks in their owning slices; existing green tests alone do not establish those new guarantees.

---

## Remediation stage 1: Execution and trust containment

### Design

Resolve F01–F06 and the immediate exposure/resource paths in F09/F11/F12. Resource ownership must last until execution has actually exited. A timeout response alone is insufficient. Network restrictions are interim containment; they do not replace authentication or artifact provenance. Owners are the execution host, gateway, module cache, wp-operator, Helm configuration, and the downstream db-operator integration.

### Tasks

- [ ] **F05 — Enforce SQL policy throughout its lifecycle.** Preserve table selectors and every declared grant field, or reject unsupported scopes. Patch existing managed credential specs; enumerate owned credentials; revoke removed users, permission downgrades, SQL removal, and final deletion even after spec edits. Coordinate with pinned db-operator `v1.1.6`: its current grant-only updates and best-effort user-drop cleanup require repair or a verified enforcement alternative. Release/pin downstream changes and test the deployed version, rather than relying on a newer local checkout. Include sequence/default privileges and existing-session behavior in the effective policy; do not treat `Ready` or CR disappearance as proof of revocation.
- [ ] **F06 — Prevent credential/resource identity collisions.** Replace ambiguous concatenated names with an unambiguous encoded/hash tuple or owner-UID identity within Kubernetes name limits. Check owner UID and expected spec before adoption, Secret use, patching, or deletion. Define a safe transition for existing names without adopting another app's resource. Extend these ownership checks to fixed-name credentials and migration resources (F16).
- [ ] **F11 — Close remaining immediate unbounded paths.** Set SQL result and query limits, Redis I/O deadlines, and per-app metric authorization/cardinality limits. Set a conservative aggregate invocation budget compatible with pod memory, including host allocations; complete capacity accounting in stage 3.
- [ ] **F18 — Begin the regression/CI gate now.** Add exact-revision build/unit checks and deterministic cancellation, race, ownership, and policy fixtures. Repair shared-source Tilt dependency inputs before relying on local regression results; expand the release gate in stage 4.

### Verification and exit gate

- [ ] Looping production components and delayed SQL/KV side effects stop within the declared cancellation bound; no pending delayed write occurs after expiry, and no new operation starts after cancellation. Account explicitly for an already-committed external operation. Actual active executions stay within the admission limit, including terminating work.
- [ ] Sustained successful traffic keeps task bookkeeping bounded and memory stable after warm-up; panic and cancellation paths release resources only after termination; shutdown completes within its budget.
- [ ] Real database checks prove table isolation, narrowed table scopes, ALL-to-SELECT downgrade, sequence/default privilege limits, existing-session revocation, user removal, SQL removal, and deletion after edits. Failed downstream role deletion retains a retryable finalizer. Two apps using the `foo-bar`/`baz` and `foo`/`bar-baz` collision pair cannot share or delete each other's credentials; test both deletion orders.
- [ ] Oversized/slow input and dependency results terminate or reject within finite limits; cross-app metric use is denied. The e2e suite and new negative security tests pass.

---

## Remediation stage 2: Configuration and lifecycle convergence

### Design

Address F07–F10/F13 and the immutable-identity and migration portions of F12/F16. Persist desired and serving ApplicationRevisions, then project them to assigned pools and the gateway. Coordinate preparation, activation and retirement; exclude unprepared hosts and retain authority only under a valid lease. Owners are wp-operator, execution host, gateway, protocol definitions and db-operator integration.

### Tasks

- [ ] **F12 — Fence serving artifact identity at activation.** Bind persisted desired digests to participating host and gateway activation acknowledgements; record serving digests separately from desired module resolution. Never mutate a serving generation during automatic tag tracking.
- [ ] **Placement — Assign bounded execution pools.** Add explicit tenant/trust-cohort assignments, per-pool workload identities and capacity limits. Use deterministic placement and Kubernetes-managed endpoints initially. Authorize revision/credential delivery by assignment, remove global broadcasts and fence reassignment by revision and placement identity. Do not route to an endpoint until that exact revision is prepared; drain old assignments and revoke their access within the declared bound.
- [ ] **F07 — Stage and atomically publish generations.** Fence module/pool insertion and eviction by Application UID and desired generation/revision. Late old loads, deleted functions, and delete/recreate under the same name cannot resurrect state. Prepare dependencies before activation and serving acknowledgments; distinguish received/preparing/applied/failed status. Retry transient preparation failures with bounded backoff while retaining the old working generation only under the documented compatibility policy. Revoke removed privileges promptly even when retaining old code; security downgrades must not wait indefinitely for unrelated preparation.
- [ ] **F08 — Make snapshot/watch delivery replayable.** Generate assignment-scoped host configuration and gateway routes from durable revision state, using one sequence/epoch scheme, coherent snapshot watermarks, ordered replay and gap recovery. Registering first without ordering updates around the snapshot is insufficient. Clean stream closure triggers reconnect/resync without discarding still-authorized serving state.
- [ ] **F09 — Verify replicated configuration authority.** Test leader-only publication with two operators, leadership transitions and replacement while keeping webhook availability separate. Cover startup and restart races under those topologies before relaxing the single-serving-replica restriction.
- [ ] **Authority leases — Bound disconnected serving.** Specify duration, renewal cadence, conservative clock/restart handling and maximum revocation delay. Only current leadership may issue or renew leases for exact assignments/revisions; replay and stale leaders cannot renew them. Preserve committed service during short sync outages, deny new admission and cancel affected work at expiry/revocation, and require fresh authority after an unverifiable restart. Distinguish degraded control connectivity from data-plane readiness.
- [ ] **F10 — Own HTTP routes and remove them completely.** Persist owner identity, reject duplicate paths across apps/namespaces and within one app, reserve probe paths, and use owner-checked deletions. Keep admission and reconciliation checks consistent, including concurrent admissions. Apply atomic old/new route diffs with tombstones for rename, function removal, HTTP-to-topic conversion, and Application deletion; snapshot order must never select a conflicting winner.
- [ ] **F13 — Reconcile credential rotation and live readiness.** Watch assigned NATS credentials and Secret/PostgresCredential changes, reload reconnect credentials and compare complete SQL bindings on deltas/snapshots. Replace changed pools safely and remove obsolete users/pools. Derive readiness from prepared assigned revisions, valid authority and required data-path dependencies; report sync degradation separately. An unrelated broker outage must not make HTTP-only functions unavailable.
- [ ] **Data bindings — Separate durable data from deployment identity.** Add independently owned data resources and explicit Application bindings with authorization checks. Migrate existing namespace/name-derived stores through verified mappings without copying, deleting or replacing retained data. Test application rename/recreation, explicit rebind, conflicting adoption and orphan cleanup. Revision-scoped credentials and migration runs reference the stable data identity; application deletion removes access, not retained data.
- [ ] **F16 — Fence migration and capability transitions.** Verify the exact deployed db-operator contract, including observed generation, target/current revision, and observed immutable artifact. Require readiness for the desired migration generation rather than accepting an old `Ready`. Order bootstrap so credentials scoped to migration-created tables cannot block creation of the migration that creates those tables. Define whether old traffic is drained or retained during pending/failed migrations and what schema compatibility is required. Enforce removal/revocation and ownership-safe finalization across update/remove/recreate, with explicit data-retention and downstream-deletion completion rules.

### Verification and exit gate

- [ ] Barrier-controlled tests reorder old/new loads and pool preparation, delete during load, delete/recreate, fail/recover dependencies, and send traffic through activation. Only the desired prepared generation serves; transient failures retry without lost messages silently masquerading as readiness.
- [ ] Create/update/delete in the snapshot-stream handoff gap, revision gaps, reconnects, and operator restarts converge without a manual restart. Slow-client resync and epoch changes cannot overwrite newer state.
- [ ] Multiple operators, startup before reconciliation, leadership change, and replacement cannot publish an uninitialized empty authoritative snapshot or erase working state.
- [ ] Pool scale/reassignment under load cannot leak another assignment's configuration, secrets or traffic. New hosts prepare exact revisions before routing; old assignments drain and lose authority. Application growth beyond one host's inventory limit is handled by placement or explicit capacity rejection.
- [ ] Short control-plane outages retain the committed revision within its lease. Expiry, explicit revocation, stale leader renewal, replayed leases, clock uncertainty and restart fail closed within the specified bound; no stale endpoint is readmitted without fresh authority.
- [ ] Conflicting routes never cross owners. `/old` stops serving after rename to `/new`; removal, trigger conversion, and deletion remove routes without a gateway restart.
- [ ] NATS rotation, PostgreSQL rotation while disconnected, removed users after full resync, broker outage/recovery, and clean stream closure produce the correct readiness and credentials. Stale migration status, artifact/target changes during a running migration, and failed migrations on already-serving apps cannot activate an unready generation. Initial table-scoped credentials and their table-creating migration converge without deadlock. Tag movement cannot place digest B under digest A; replicas agree on one immutable generation.

---

## Remediation stage 3: Resource budgets and failure behavior

### Design

Complete F11–F17 after execution termination and generation convergence are dependable. Move synchronous HTTP onto the direct transport and new guests onto versioned interfaces. Use finite budgets across queueing, streaming, compilation, host calls and shutdown; retain explicit Core NATS live-event semantics. Owners are execution host, gateway, optional module cache, wp-operator, shared runtime libraries and Helm configuration.

### Tasks

- [ ] **F11 — Define and enforce total capacity.** Budget request/response headers, buffered bodies, stream windows and total bytes/time, SQL rows/result bytes and pool acquisition, KV payloads/I/O, host-call concurrency, guest memory/table/resource counts, queues and aggregate resident memory. Include copies, shared compiled modules, metric series and terminating invocations. Enforce per-Application fairness within each assigned pool; reconcile limits with pod budgets and SQL connections across replicas/users.
- [ ] **F11/F15 — Scope and reconcile metrics.** Authorize metric calls by owning Application, cap label size/series cardinality, retire removed definitions/series, and compare full type/label schemas. Implement safe schema updates or explicitly reject mutation at admission; updates must not silently depend on restart.
- [ ] **F12 — Complete preparation lifetime and memory accounting.** Integrate queued and active preparation into generation cancellation and shutdown. Retain accounting for replaced compiled components until accepted invocations release them. Budget compiler peak memory and serialization copies alongside registry storage; isolate or terminate over-budget compilation without prematurely releasing admission.
- [ ] **Artifacts — Deduplicate local compilation and remove the mandatory cache hop.** Key immutable compiled components by Wasm digest and engine compatibility settings, share in-flight preparation, and keep per-Application bindings/secrets separate. Retain code while generations/invocations reference it, with bounded eviction and peak-memory accounting. Support verified OCI operation without the raw-cache service; keep that service optional and test absence, failure and corruption. Do not accept downloaded native artifacts.
- [ ] **F14 — Implement direct HTTP dispatch.** Choose HTTP/2 or gRPC, document the wire envelope and use authenticated assignment-scoped prepared endpoints. Carry invocation ID, revision, deadline and trace context through gateway, queueing, execution and host calls; propagate disconnect/cancellation. Bound discovery/connect/reply waits, define overload/unavailability/timeout responses, preserve repeated headers and reject unsafe framing. Remove NATS from HTTP after compatibility tests pass; prohibit automatic retries of non-idempotent requests.
- [ ] **Guest API — Version contracts and adopt WASI HTTP.** Add explicit version/compatibility validation and a WASI HTTP world for new guests. Keep existing compiled guests usable through a bounded adapter; reject unsupported versions before activation. Version custom SQL/KV/messaging/metrics capabilities with typed denial/timeout/overload/dependency errors, authorized resource handles and explicit byte/row limits. Add cursors/streams where needed; bound their lifetime and cancellation. Validate repeated headers, body streaming and client disconnects; WebSockets remain a separate feature decision.
- [ ] **Invocation identity — Correlate both data paths.** Generate request/invocation IDs at trusted ingress, propagate trace context across direct HTTP and event headers, and attach revision, assignment and function identity to platform/guest logs. Bound and validate incoming context. Keep IDs out of metric label sets; test correlation through host calls without exposing secrets.
- [ ] **F15 — Honor declared guest environment.** Populate each invocation's WASI context from declared Application environment in both HTTP and message worlds. Do not inherit host environment or leak values across applications.
- [ ] **F16 — Validate identifiers and lifecycle inputs.** Enforce unique function/user/metric names, safe resource and NATS identifiers, reserved prefixes/paths, and supported grant scopes at admission and reconciliation. Test already-running app failures, ownership mismatches, capability removal, and finalizer retry behavior rather than create-only success.
- [ ] **F17 — Bound shutdown and stream/session lifetimes.** Stop intake, drain accepted queued and active work under one finite grace budget, then cancel/terminate remaining work under stage 1's contract. Add gateway graceful shutdown and bounded operator stop. Make idle stream waits/ACKs cancellation-aware with deadlines; use registration session tokens so old cleanup cannot remove a replacement session. Specify counts/status for rejected, dropped, and unfinished work.
- [ ] **F17 — Enforce the live-event contract before topic expansion.** Document and test Core NATS as best-effort live events, independently of synchronous HTTP. Durable jobs require their own explicit API and acknowledgment, retry, idempotency and dead-letter policy before implementation. Provisioned JetStream storage alone supplies none of those guarantees; do not promise exactly-once external side effects.
- [ ] **F11–F17 — Complete operational coverage.** Extend lifecycle metrics across components; expose preparation/retry state, serving revision/digests, pool connection counts, metric cardinality, and cache occupancy. Define alerts from the documented budgets, readiness transitions, and recorded lifecycle history.

### Verification and exit gate

- [ ] Slow/oversized bodies, serialized responses, large SQL results, blocked Redis/SQL, label growth, and cold-start bursts reject or finish within bounds without unbounded process growth or exceeding documented connection budgets.
- [ ] Same-digest functions share compilation without sharing invocation state; changed engine settings do not reuse incompatible code. Tag races, optional-cache absence/stalls/corruption, large artifacts and eviction cannot strand activation or admit unverified code.
- [ ] An 11-second reply succeeds under a 30-second configured deadline; real expiry returns 504. Direct HTTP works without NATS, targets only prepared assigned revisions, propagates disconnect/cancellation and never silently retries a side effect. Multiple cookies and bounded streamed bodies survive the round trip; unsafe headers are rejected.
- [ ] Existing guest binaries and new versioned WASI HTTP guests pass the same behavioral fixtures. Unsupported versions fail preparation; typed capability errors, oversized SQL/KV results and abandoned cursors/streams stay within limits. Both invocation worlds receive only their declared environment; metric schema reuse and invocation/trace correlation work without restart or cross-tenant leakage.
- [ ] Filled queues at SIGTERM, looping guests, idle disconnects, missing ACKs, and overlapping reconnects exit within the declared grace period. Accepted messages follow the chosen delivery policy; readiness reflects outage/recovery and every dropped/unfinished item is accounted for.

---

## Remediation stage 4: Reproducible development and releases

Extract runtime boundaries as their owning slices are implemented; complete the local workflow before this stage's exit gate.

### Tasks

- [ ] **Shared runtime — Use one implementation locally and in-cluster.** Extract engine setup, guest execution, capability interfaces, budgets and cancellation into a reusable library. Keep Kubernetes/configuration transport and concrete dependency adapters outside the runtime core. Add cluster-free `wp run` and `wp test` using local configuration and explicit local backend adapters; do not emulate guest behavior in a second runtime or require projected cluster credentials locally.
- [ ] **Local adapters — Serve HTTP and preserve capability behavior.** Add a loopback HTTP server and in-memory KV/live-event adapters with application isolation, atomic operations, declared delivery behavior and the same authorization/limits/cancellation as deployed execution. Run these applications without Docker; expose unsupported local features explicitly.
- [ ] **Local SQL — Manage PostgreSQL on demand.** Accept an explicit existing database URL without requiring Docker, or start/reuse a CLI-owned project-scoped container with a compatible pinned PostgreSQL version and allocated loopback port. Bound startup/readiness waits, provision databases/users/grants, apply migrations and inject credentials safely; failed setup must not report readiness. Coordinate concurrent sessions, stop the managed database after its last session exits and retain development volumes; provide an explicit owned-data reset. Never mutate unrelated Docker resources or stop/reset an external database. Isolate `wp test` data from persistent development data.
- [ ] **F18 — Establish canonical schema generation.** Establish one canonical CRD generation/deployment path and a generated-source/schema drift check; do not hand-edit generated outputs.
- [ ] **F18 — Complete release validation.** Extend the local Tilt CI gate with reproducible generators and schema/protobuf/WIT compatibility checks. Align Makefile commands and remove misleading envtest/integration claims. Pin tool/dependency versions and verify release artifacts through the same real-cluster tests. Document adapter compatibility and upgrade/rollback for stored revisions and legacy data bindings.
- [ ] **F01–F18 — Make failure tests release gates.** Include the cases above in automated regression coverage, prioritizing F01–F13 and F17. Cover load/soak, update/reconnect/rollout, secret redaction, SQL/metric isolation, migration status, and shutdown. Do not substitute line-coverage percentages for verified failure contracts.
- [ ] **Documentation — Reconcile claims with implementation.** Update the owning README/architecture/contribution/component docs as fixes land. Distinguish implemented guarantees, development-only defaults, planned features, and measured results. Correct credential rotation, gateway settings, SQL/Redis topology and connections, topic publish policy, admission, deletion/retention, migration rollback, and wire contracts; link to source schemas rather than duplicate them. Mark CLI commands unimplemented while the component is only a README. Remove unsupported cold-start/concurrency/pooling/mmap/HPA/KEDA claims from current-state docs; keep goals here until measured or implemented.
- [ ] **Onboarding — Prove a fresh clone.** Provide cluster-free HTTP/KV/messaging instructions without Docker and a one-command local SQL path with Docker or an existing PostgreSQL URL. Document persistence/reset, startup failures and test isolation. Keep a separate deployed HTTP/SQL path covering guest targets, module/migration artifacts, KUBECONFIG and retained data. Align contribution guidance with the workspace/umbrella chart. Resolve an explicit license before outside adoption.
- [ ] **SQL pooling — Restore warm connection reuse.** Return healthy execution-host PostgreSQL connections to the pool after confirmed COMMIT/ROLLBACK and bounded session-state cleanup, so function invocations benefit from warm connections. Close cancelled, terminated, or uncertain connections and retain admission permits through cleanup. Verify backend reuse across successful calls, transaction/session isolation, and cancellation safety; measure connection churn and warm SQL latency.
- [ ] **Performance and operations — Measure after correctness gates.** Define target workloads and pool sizes, then measure warm/cold latency, throughput, aggregate memory, SQL fanout, compilation reuse, registry traffic and outage recovery. Use those results to justify optional shared caching or more elaborate placement. Test backup/restore and upgrade/rollback compatibility; prepare budget/lease/readiness alerts, incident and rotation runbooks, and dependency/security maintenance before claiming production readiness.

### Verification and exit gate

- [ ] A clean clone runs/tests a guest without Kubernetes and completes the separate deployed HTTP/SQL path. Local and deployed runtimes agree on guest results, limits, environment, capability denial and cancellation. Shared edits rebuild consumers; regeneration is reproducible with no schema or guest-contract drift.
- [ ] HTTP/KV/messaging run with Docker unavailable. SQL starts a managed database only when needed, also works with an explicit external URL, and reports startup failures within its deadline. Database grants/migrations match the deployed contract; development data survives restart. Concurrent projects/sessions and tests remain isolated, shutdown retains volumes, and reset affects only explicitly selected CLI-owned data.
- [ ] The release pipeline runs the production invocation path and required negative/race/transition tests against the release artifacts; generated artifacts, Helm validation, tagged integration, and `e2e-tests` all pass.
- [ ] Current-state documentation matches demonstrated behavior. Performance claims include workload, hardware, configuration, and measured distributions; readiness claims meet the gates above.

---

## Phase 13: Middlewares

Deferred until remediation stages 1–4 pass. Middleware must use the authenticated artifact path, immutable generation activation, owner-scoped capabilities, and one cancellation/resource budget for the entire request/response chain. Explicitly authorize global publication and cross-Application use; a shared compiled component must not share invocation secrets, context, or permissions.

### Design

Add composable middleware support for HTTP functions using the versioned WASI HTTP boundary and shared runtime. Middleware components run in-process under the same invocation budget; no additional network hop is required.

A function with `trigger.middleware` (and `type: http`) is a middleware module rather than a handler. Middlewares can be scoped to their own Application (default) or declared `global: true`, making them referenceable by name from any Application's middleware chain. The operator enforces cluster-wide uniqueness for global middleware names. An app-local middleware with the same name as a global middleware shadows the global one within that application — local always wins. Middlewares are composed at two levels: top-level `spec.middlewares[]` applies to all HTTP functions in the Application; `spec.functions[].middlewares[]` applies to a specific function. The effective chain for a request is `spec.middlewares` + `spec.functions[].middlewares` + handler, in order. `on-response` is called in reverse order after the handler returns.

Define request/response hooks, typed abort outcomes and bounded shared/private context in a new versioned middleware contract. Use standard HTTP headers for cookies. Preserve existing HTTP binaries through the compatibility adapter; middleware must not require an in-place change to their export signature.

### Tasks

- [ ] Define a versioned middleware contract compatible with the selected WASI HTTP model, including typed aborts and bounded context. Document body ownership, streaming and cancellation across hooks; preserve the legacy guest adapter.
- [ ] Update the execution host to chain middleware modules for HTTP invocations: call each middleware's `on-request` in order, passing accumulated request and response; on abort variant, return the response or generate a 5XX immediately; call each middleware's `on-response` in reverse order after the handler returns.
- [ ] Add `trigger.middleware` to `Application` CRD `FunctionSpec`: fields `type` (enum, only `"http"` for now) and optional `global` (bool, default false).
- [ ] Add `spec.middlewares[]` (ordered list of middleware names, applied to all HTTP functions) and `spec.functions[].middlewares[]` (ordered list of middleware names for a specific HTTP function) to the `Application` CRD.
- [ ] Operator: enforce cluster-wide uniqueness for global middleware names (`MiddlewareConflict` status condition, covered by the ValidatingWebhook once in place).
- [ ] Include middleware owner UID, immutable digest, API version, capability bindings and chain order in ApplicationRevision. Resolve local names before global names and authorize cross-Application references. Share compiled code by digest/engine compatibility while keeping owner-scoped bindings separate; retain code through all referencing generation/invocation leases.
- [ ] Publish middleware revisions only to assigned pools and prepare the entire chain before activation. Reuse verified OCI loading and the bounded local code cache; shared raw caching remains optional.
- [ ] Define middleware-owner versus consuming-Application authority for SQL, KV, environment, messaging, and metrics; authorize global publication/use explicitly. Sharing a compiled `Component` must keep invocation Stores, secrets, context, and capabilities isolated. Reject missing/invalid owner identity for global middleware rather than falling back to consumer credentials.
- [ ] Bound chain length and shared/private context bytes; use one deadline and aggregate resource budget across all `on-request`, handler, and `on-response` calls. Define unwind behavior for abort, trap, and cancellation; stage the entire chain in one immutable generation.
- [ ] Add versioned middleware examples and run existing unmodified HTTP binaries through the compatibility adapter alongside new WASI HTTP guests.
- [ ] Add e2e test: deploy a global auth middleware and an HTTP function with it in its chain; verify that a request without a valid auth token gets a 401 from the middleware (chain aborted), and a valid request reaches the handler.
- [ ] Add regressions for cross-app secret/capability denial, shared-module state isolation, a looping middleware, oversized context/chain, and update/delete during chain preparation. Verify handler side effects do not start after cancellation.
- [ ] Update `framework/runtime.wit` documentation comments; update the project README and `docs/architecture.md` to document the middleware execution model and the two-phase (`on-request` / `on-response`) contract.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. Verify auth rejection before handler side effects, successful request/response hooks, bounded streaming/cancellation and isolated cross-Application capabilities. Existing guest binaries and new WASI HTTP guests must both pass through the direct HTTP path.

---

## Phase 14: Static file serving

Deferred until remediation stages 1–4 pass. Reuse authenticated, revision-safe configuration, immutable OCI identity, finite artifact budgets, and ownership-safe route activation/deletion.

### Design

Add a `trigger.static` function trigger type to the `Application` CRD, enabling frontend static assets to be served from an ORAS artifact reference. This allows the full vertical slice of an application — frontend and backend — to be declared in a single `Application` CRD.

Reuse artifact verification, ApplicationRevision publication, prepared-endpoint routing and authority leases for static content. Stage verified bundles in bounded storage and activate them with their owning revision. Choose the serving placement within existing components first; introduce a separate static-host deployment only if measured load or isolation requires it. A new parallel configuration-sync protocol is not a prerequisite.

Static paths are subject to the same cluster-wide uniqueness enforcement as HTTP trigger paths (`StaticPathConflict` condition; included in the validating webhook scope).

### Tasks

- [ ] Add `trigger.static` to the `Application` CRD `FunctionSpec`: `path` (URL path prefix, unique cluster-wide) and `artifact` (ORAS artifact reference to the static bundle).
- [ ] Add static bundle identities, owned routes and preparation status to the existing revision/publication contract. Select serving placement and reuse the artifact adapter; stage files before revision-fenced route activation.
- [ ] Enforce download/extraction/file-count/expanded-size/disk-cache budgets and deadlines; reject archive traversal, unsafe symlinks, and requests escaping the serve root. Define overlapping static/HTTP prefix ownership and reserved paths; evict unused generations safely after draining.
- [ ] Publish bundles only to assigned serving endpoints and track exact prepared revision acknowledgements. Recover through the existing snapshot/replay contract.
- [ ] Gateway: route static paths only to prepared authorized endpoints, with bounded response streaming and disconnect propagation.
- [ ] Update Helm storage and security settings for the selected placement; add a separate Deployment/Service only if the isolation or load decision requires it.
- [ ] Add `StaticPathConflict` status condition to `Application` (parallel to `TopicConflict`) and include static path uniqueness in the validating webhook scope.
- [ ] Update wp-operator README: document `trigger.static`, serving behavior and path ownership rules.
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

Shared call-response topics are an optional messaging feature, independent of the direct HTTP path. Before enabling them, define explicit responder and observe-only roles. The proposed baseline is one authorized responder per request topic, with observers unable to reply; validate this across assigned pools. A host-local first-reply rule cannot arbitrate independent hosts or prevent duplicate side effects. Specify observer side effects, reply timeouts, retries and idempotency before removing responder ownership checks.

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

- **Status:** desired/prepared/serving revision, readiness, pool assignment, serving-authority expiry/degradation, authorized data bindings and migration status, desired/serving immutable digests and prepared module size. Resource metadata must not expose credentials.
- **Logs:** a streaming Loki view scoped to the authorized Application/function, with invocation and trace correlation across revisions and assigned pools. The API server constructs scoped LogQL queries.
- **Metrics:** invocation count, error rate and p99 latency across the function's assigned pools/revisions. The API resolves authorized instrument identity and schema; a bare metric name is not sufficient authorization. It constructs bounded PromQL queries server-side.

**Architecture:**
- `components/wp-ui/` — a Go binary with two responsibilities: (1) serve the compiled SPA from an embedded `embed.FS`; (2) expose an authenticated JSON REST API (`/api/applications`, `/api/logs`, `/api/metrics`) that the SPA calls. Give its in-cluster Kubernetes client only the required read-only RBAC, and independently authorize each user's namespace/Application access. Construct scoped Loki/Prometheus queries server-side and keep credentials out of browser responses.
- The SPA is built from a `web/` directory inside `components/wp-ui/` using a standard Node.js build step (Vite or similar). The build output is embedded at compile time via `go:embed`.
- Helm chart: add `wp-ui` Deployment, Service and least-privilege read-only RBAC for the Application, revision, assignment and data-status resources shown by the UI, excluding Secrets. Loki and Prometheus endpoints are configurable via Helm values.

### Tasks

- [ ] Scaffold `components/wp-ui/`: Go module, `main.go`, `web/` SPA skeleton (Vite + TypeScript), Dockerfile, and Tiltfile resource.
- [ ] Implement user authentication and namespace/Application authorization; restrict Kubernetes reads and telemetry proxy access, redact secret-bearing fields, and bound query ranges, results, concurrency, and rates.
- [ ] Implement `/api/applications`: return only authorized application/revision metadata, including desired/serving identity, assignment, authority state, data/migration status, immutable module identities, trigger types and feature flags. Read no secret values.
- [ ] Implement the graph view SPA page: render functions and middlewares as nodes, middleware chain and topic edges as directed edges; colour-code by readiness.
- [ ] Show status and feature flags (KV, SQL) as node annotations in the graph.
- [ ] Implement the Status tab: display desired/prepared/serving state, assignment and authority degradation, authorized data/migration status, readiness conditions and immutable artifact metadata.
- [ ] Implement the `/api/logs` proxy endpoint: authorize `namespace`, `app`, and `function`, validate/escape labels and bounded time-range parameters, construct a scoped LogQL query, and stream bounded NDJSON responses with dependency deadlines.
- [ ] Implement the Logs tab in the detail panel: call `/api/logs` and render a live-scrolling log view.
- [ ] Implement `/api/metrics`: resolve authorized function/instrument selections across assigned pools and revisions, enforce schema ownership and bounded time ranges, and return bounded series with finite dependency deadlines.
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

## Future Work: Distributed Tracing Export (OpenTelemetry)

Build on stage 3's transport-neutral invocation and trace context. Add trace export without changing execution ownership or making serving depend on collector availability.

### Tasks

- [ ] Integrate OpenTelemetry spans across gateway → direct HTTP → execution host → host functions and the separate NATS event path, using the existing context contract.
- [ ] Add OpenTelemetry exporter configuration (OTLP endpoint, sampling rate) as env vars.
- [ ] Bound exporter buffering and verify sampling, collector outages and log–trace correlation without adding high-cardinality metric labels.

---

## Future Work: Circuit Breakers

Add circuit-breaker logic to dependency adapters (registry/optional cache, database pools, direct HTTP and NATS) so that sustained failures trigger fast-fail rather than timeout accumulation.

### Tasks

- [ ] Evaluate circuit-breaker crate options (e.g. `again`, `backon`, or a thin custom wrapper).
- [ ] Apply circuit breakers to selected dependency adapters and verify recovery without overriding revision authority or cancellation.
- [ ] Surface circuit state (closed/open/half-open) as a Prometheus metric.

---

## Future Work: Durable Jobs

Add a distinct durable-job API after remediation stages 1–4. Keep live events and synchronous HTTP behavior explicit and unchanged by opting into jobs.

### Tasks

- [ ] Define job identity, retention, authorization, retry/backoff, attempt limits, cancellation and dead-letter behavior before selecting JetStream consumer settings.
- [ ] Implement acknowledged publication/consumption with bounded outstanding work and application idempotency requirements; do not claim exactly-once external effects.
- [ ] Test crash-after-side-effect/before-ack, duplicate delivery, replay, poison jobs and reassignment/revision changes under the declared policy.
