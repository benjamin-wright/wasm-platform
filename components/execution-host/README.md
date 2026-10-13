# execution-host

The runtime engine of the wasm platform: receives per-application configuration from the wp-operator, loads WASM modules from the module cache, subscribes to NATS subjects, and invokes modules with scoped host functions.

## Configuration Sync

On startup (or desync) the execution host requests a full configuration snapshot from the wp-operator via gRPC. It then maintains a bidirectional stream for incremental deltas. See [`proto/configsync/v1/configsync.proto`](../../proto/configsync/v1/configsync.proto) for the schema. Rust stubs are generated at build time via `build.rs` / `tonic-build`.

## Module Loading

When a new or updated application config arrives, the execution host:

1. Checks the module cache for a precompiled `.cwasm` keyed by `(digest, arch, wasmtime_version)`.
2. On hit, loads directly. On miss, pulls the raw `.wasm` OCI artifact, AOT-compiles it, pushes the result back to the cache, then loads it.

## NATS Message Handling

The execution host connects to the shared NATS instance using credentials read from files at `NATS_CREDENTIALS_PATH` (a Kubernetes secret volume mount). Credentials are re-read on every connection attempt, so rotations are picked up without pod restarts.

It subscribes to per-application subjects (the fully-prefixed topics pushed by the operator) using NATS **queue subscriptions**, with the subject name as the queue group. This ensures each message is delivered to exactly one replica when multiple execution-host pods are running, preventing duplicate invocations during horizontal scaling or rolling updates. When a replica's NATS connection closes (e.g. on pod termination), it leaves the queue group automatically.

On each message:

1. **`message-application`** — payload is passed to the module's `on-message` export. If the NATS message has a reply subject, the response bytes are published back.
2. **`http-application`** — the platform-private JSON payload is decoded into typed WIT records, passed to `on-request`, and the returned `HttpResponse` is serialised back to JSON for the NATS reply.

`MAX_CONCURRENT_INVOCATIONS` bounds both execution slots and retained task records (default 64 each). Execution slots cover scheduled guest work through actual guest exit and backend cleanup, including timeout and panic paths. Task records also include reply publication and finished tasks awaiting collection, so slow replies can reduce execution throughput. Reply publication has a two-second timeout.

Completed records are collected continuously, including while idle or waiting for an execution slot. Task failures and panics are reported as records are collected. The host-owned waiting backlog is bounded at 257 messages: a 256-entry channel and one message waiting for admission. This excludes buffering inside NATS clients and the broker.

## Invocation Deadlines

One `WASM_TIMEOUT_SECS` budget starts when a message is forwarded from its NATS subscription into the dispatch queue and covers channel/permit queueing, instantiation, guest execution, and host operations. Expired queued messages do not run. CPU loops and WASI waits stop on expiry; a timeout response is sent after execution exits. Guest stdout/stderr are discarded; use the `log` interface for application logs.

Deadline checks use monotonic time. CPU execution yields at epoch checks, driven every 10 ms; this is cooperative interruption, subject to operating-system scheduling. SQL cleanup adds at most two bounded 2-second waits. Admission accounting includes this terminating work. These are scheduling and cleanup budgets, not a real-time scheduling guarantee.

SQL calls accept one parsed statement: queries, INSERT/UPDATE/DELETE, CREATE TABLE/INDEX, ALTER TABLE, DROP, or TRUNCATE. Each call runs in a host-owned transaction and commits independently. Guest transaction control, standalone session-setting statements, procedures, prepared statements, and multi-statement input return an error. Cancellation before commit submission prevents transactional writes from committing; cleanup terminates the backend through a separate connection and closes the data connection. If termination cannot be confirmed, the host reports that uncertainty. PostgreSQL nontransactional effects, including sequence advancement, are not rolled back.

KV mutations check a server-time deadline atomically with the mutation. A command delayed until that deadline is rejected. The deadline subtracts the TIME request round trip and assumes Redis wall time advances with host monotonic time; backward changes to the Redis clock can weaken the bound. Cancellation also drops the private client connection.

Writes acknowledged before cancellation remain committed. A SQL COMMIT already submitted, a KV mutation admitted before its backend deadline, or a NATS publish already enqueued may complete despite cancellation; their outcome can be uncertain if the reply is lost. Timeout is not an undo operation, and retries must account for those submitted effects. No new guest operation starts after observed cancellation.

## Data Isolation

- **PostgreSQL** — per-app, per-user connection pools keyed by `(namespace, app_name, username)`, created eagerly on config arrival. The operator assembles full connection URLs (with embedded credentials) and delivers them via the config stream; the host never reads PostgreSQL credentials separately. Each invocation receives the pool for its function's assigned SQL user, or `None` if the function has no SQL access — SQL calls return `Err` in that case.
- **Redis** — private asynchronous connection for each host call to the shared instance. Keys are transparently prefixed with `<namespace>/<app>/` (derived automatically from the application identity — no CRD field required). The `store` parameter has been dropped from the `kv` WIT interface; apps that need sub-namespacing can prefix their own keys.
- **NATS** — each app is bound to its own subject. The `messaging` host function prefixes the caller-supplied topic with `fn.` and publishes to that subject; modules can send to any platform topic, not only their own.

## Logging

Guest modules emit log entries via the `log` WIT interface (`log::emit(level, message)`). The host forwards each call to the platform's `tracing` subscriber with `app_name` and `app_namespace` as structured fields, making application log output distinguishable from platform infrastructure logs. Log calls are fire-and-forget from the guest's perspective.

Connection diagnostics report the operation and error category without connection URLs, credentials, or remote error text. Configuration debug output excludes connection credentials and environment values; HTTP payload debug output excludes paths, query strings, headers, and bodies. Unexpected invocation failures log application identity and return HTTP `500` with `internal error: invocation failed`.

## Environment Variables

| Variable | Description |
|---|---|
| `CONFIG_SYNC_ADDR` | gRPC address of the wp-operator (required). |
| `MODULE_CACHE_ADDR` | HTTP address of the module cache (required). |
| `NATS_CREDENTIALS_PATH` | Directory containing NATS credential files (required). |
| `REDIS_URL` | Shared Redis URL (e.g. `redis://redis:6379`). |
| `PG_POOL_MAX_CONNECTIONS` | Maximum connections per per-user PostgreSQL pool (default `5`). |
| `MAX_CONCURRENT_INVOCATIONS` | Execution and retained-task limits per host (default `64`, must be positive and fit in `u32`). |
| `WASM_TIMEOUT_SECS` | Shared invocation deadline in seconds, including queueing (default `30`, must be positive). |
| `WASM_FUEL_LIMIT` | Maximum fuel units per invocation; unset disables fuel metering (optional). |
| `WASM_MEMORY_LIMIT_MB` | Maximum linear memory per module in MiB (default `64`). |
| `HOSTNAME` | Used as `host_id` in gRPC (injected by downward API). |

## Metrics

The execution host exposes a Prometheus-compatible `/metrics` endpoint on **port 9090**.

Two classes of metrics are served:

**User-defined** — registered from `spec.metrics` on config arrival.  Guests call `counter-increment` or `gauge-set` via the `metrics` WIT interface.  The host injects `app_name` and `app_namespace` labels on every series; guests supply any additional labels declared in the spec.

**Platform** — emitted by the host itself, independent of any Application spec.

| Metric | Type | Labels |
|---|---|---|
| `wasm_host_module_compilations_total` | Counter | `app_name`, `app_namespace`, `result` (`ok`/`err`) |
| `wasm_host_events_received_total` | Counter | `app_name`, `app_namespace`, `trigger` (`http`/`topic`) |
| `wasm_host_messages_sent_total` | Counter | `app_name`, `app_namespace` |
| `wasm_host_kv_reads_total` | Counter | `app_name`, `app_namespace` |
| `wasm_host_kv_writes_total` | Counter | `app_name`, `app_namespace` |
| `wasm_host_http_requests_received_total` | Counter | `app_name`, `app_namespace`, `status` |
| `wasm_host_dropped_metric_calls_total` | Counter | `app_name`, `app_namespace`, `reason` (`unknown_metric`/`wrong_labels`) |

Invalid guest metric calls (unknown name or mismatched label keys) are silently dropped and logged at error level; `wasm_host_dropped_metric_calls_total` is incremented with the appropriate `reason`.

Dispatch metrics describe host-owned lifetimes, using only fixed labels. Their names have the prefix `wasm_host_dispatch_`:

| Suffix | Type | Meaning and labels |
|---|---|---|
| `work` | Gauge | `state`: `queued`, `tracked`, `completed_uncollected`, `running`, or `terminating`. |
| `limit` | Gauge | Configured capacity; `kind`: `queued`, `execution`, or `tracked`. |
| `high_water` | Gauge | Highest lifetime occupancy; same `kind` values. `execution` combines running and terminating work. |
| `capacity_breaches_total` | Counter | Occupancy above its configured limit on admission or configuration; `kind` as above. |
| `accounting_errors_total` | Counter | Unbalanced lifetime accounting; `state` as for `work`. |
| `admissions_after_stop_total` | Counter | Task admissions after intake has stopped. |
| `tasks_finished_total` | Counter | Task wrappers that exited; `outcome`: `success`, `timeout`, `error`, `panic`, `cancelled`, `rejected`, or `other`. |
| `tasks_reaped_total` | Counter | Records collected by dispatch; `outcome`: `ok`, `panic`, `cancelled`, or `other`. |
| `queue_exits_total` | Counter | Messages leaving the waiting backlog; `outcome`: `admitted`, `expired`, `unknown_route`, `module_unavailable`, `error`, `shutdown`, or `other`. |
| `queue_wait_seconds` | Histogram | Waiting lifetime through admission, rejection, or dropping. |
| `execution_seconds` | Histogram | Execution lifetime through guest exit and backend cleanup. |
| `cancellation_to_exit_seconds` | Histogram | Time from observed cancellation until guest and cleanup exit. |
| `drain_seconds` | Histogram | Time from stopping intake until subscriptions, tasks, and execution drain finish. |
| `draining` | Gauge | `1` while intake is stopped and drain is unfinished; otherwise `0`. |

Queue, task, and execution ownership update the counts when their lifetimes end, including panic and cancellation paths. `tracked` includes execution, reply publication, and completed records; it overlaps the other states. `running` includes scheduled execution and cleanup before cancellation is observed; `terminating` retains cancelled or panicking execution until exit. A collected wrapper with `outcome="ok"` can contain a failed invocation; invocation outcomes are recorded by `tasks_finished_total`.

Counters and high-water marks retain brief events between scrapes while the process is alive and reset on restart. A final event can be missed if the process exits before another scrape. Linux deployments also expose Prometheus process metrics, including `process_resident_memory_bytes`. See [monitoring](../monitoring/README.md) for collection, retained history, and review queries.

## Graceful Shutdown

On `SIGTERM` the execution host broadcasts shutdown and stops intake, including an admission wait. It closes the dispatch queue, waits for the subscription manager to remove its subscriptions, and drops queued messages without invoking them. Accepted executions retain their deadlines and permits through guest exit and backend cleanup.

Dispatch collects each remaining task record and reports errors without interrupting the drain. It then waits for every execution slot to return before reporting completion. A health-server exit also initiates this drain. Drain time follows invocation deadlines, SQL cleanup budgets, and reply timeouts; there is no separate process-wide shutdown deadline.

## Regression Tests

Use Tilt to build, deploy, and validate the host:

- `execution-host: regression-tests` checks host and shared-library code, including small metric lifetime/accounting tests; `execution-host: lint` runs Clippy.
- `execution-host: redis-cancellation-tests` checks delayed commands against the disposable Redis deployment on port 6391; trigger it after that deployment is ready.
- `e2e-tests` exercises dispatch through HTTP and message requests, reads back committed SQL/KV state through the cancellation fixture, and checks dispatch accounting after traffic.
- `execution-host: update-dependencies` is a manual dependency-resolution/build check that can update `Cargo.lock`; trigger it when host dependency declarations change, before the locked regression resources.
