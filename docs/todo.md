# TODO

Active implementation plan for the wasm-platform project.

---

## Phase 13: Middlewares

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
- [ ] Add a `ModuleKey` message to `configsync.proto` (`namespace`, `app_name`, `fn_name`) and an optional `module_key` field to `FunctionConfig`. The operator always populates `module_key` with the canonical owner's identity — for regular functions this is the function's own app; for global middlewares it is the declaring app, so all consumers carry the same key. When resolving a middleware reference, the operator checks the consuming app first (local shadows global), so a local middleware with the same name produces its own app's key, not the global owner's. The execution host uses `module_key` (falling back to the consuming app's identity if absent) as the `ModuleRegistry` in-process cache key, ensuring all apps sharing a global middleware resolve to a single `Arc<Component>`. Update `ConfigDiff.modules_to_evict` to check the shared key is no longer referenced by any remaining function before evicting.
- [ ] Operator: include middleware module refs, their resolved `ModuleKey`s, and chain order in the config pushed to execution hosts via ConfigSync; execution hosts load and AOT-cache middleware modules through the existing module-cache path.
- [ ] Update all existing `http-application` guest modules in `examples/` to match the new `on-request` signature.
- [ ] Add e2e test: deploy a global auth middleware and an HTTP function with it in its chain; verify that a request without a valid auth token gets a 401 from the middleware (chain aborted), and a valid request reaches the handler.
- [ ] Update `framework/runtime.wit` documentation comments; update the project README and `docs/architecture.md` to document the middleware execution model and the two-phase (`on-request` / `on-response`) contract.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. The suite must pass with tests confirming: (1) a global auth middleware rejects requests without a valid token with a 401 before the handler is reached; (2) a valid request passes through the middleware chain and the handler returns the expected response; (3) a middleware using `on-response` can modify the handler's response before it is returned to the caller; (4) all existing HTTP function e2e tests continue to pass after recompilation against the new WIT.

---

## Phase 14: Static file serving

### Design

Add a `trigger.static` function trigger type to the `Application` CRD, enabling frontend static assets to be served from an ORAS artifact reference. This allows the full vertical slice of an application — frontend and backend — to be declared in a single `Application` CRD.

wp-operator provisions a new `static-host` Rust component (under `components/`) that connects to wp-operator via a dedicated `StaticSync` gRPC service (parallel to `ConfigSync` for execution hosts). On receiving a config push, the static host pulls the ORAS artifact from the insecure registry, unpacks it to a versioned on-disk directory, and atomically swaps the HTTP serve root. There is no module-cache intermediary and no compile step — each static-host replica maintains its own on-disk cache. The gateway routes static-path requests to the static-host Service.

Static paths are subject to the same cluster-wide uniqueness enforcement as HTTP trigger paths (`StaticPathConflict` condition; included in the validating webhook scope).

### Tasks

- [ ] Add `trigger.static` to the `Application` CRD `FunctionSpec`: `path` (URL path prefix, unique cluster-wide) and `artifact` (ORAS artifact reference to the static bundle).
- [ ] Design and implement the `StaticSync` gRPC proto: `RequestFullStaticConfig` and `PushStaticUpdate` carrying per-path artifact references; follow the `ConfigSync` pattern.
- [ ] Create `components/static-host/`: a Rust binary with a `StaticSync` gRPC client, an ORAS pull client targeting the insecure registry, and an async HTTP file server; atomic on-disk directory swap on each artifact update.
- [ ] wp-operator: track connected static-host replicas; push incremental updates on Application create/update/delete with static functions; serve a full snapshot on static-host startup.
- [ ] Gateway: add static-path route entries pointing to the static-host Service; pass-through proxy for matched paths.
- [ ] Helm chart: add `static-host` Deployment and Service; add static-host gRPC address to wp-operator config.
- [ ] Add `StaticPathConflict` status condition to `Application` (parallel to `TopicConflict`) and include static path uniqueness in the validating webhook scope.
- [ ] Update wp-operator README: document `trigger.static` fields, static-host behaviour, and path uniqueness rules.
- [ ] Add e2e test: deploy an `Application` with both a `trigger.http` WASM function and a `trigger.static` function; verify assets are served at the static path and the WASM function responds on its HTTP path.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. The suite must pass with tests confirming: (1) static assets declared in `trigger.static` are served at the configured path prefix; (2) WASM HTTP functions in the same Application continue to respond correctly; (3) applying an `Application` with a static path that conflicts with an existing one is rejected at admission; (4) deleting the Application removes the static routes from the gateway.

---

## Phase 15: Multi-Subscriber Topics

### Design

Remove the per-topic uniqueness requirement so that multiple functions across different Applications can subscribe to the same NATS topic. Each function's consumer group identity is derived from its fully-qualified name (`{namespace}-{app_name}-{function_name}`) rather than the topic name, ensuring independent message delivery to each subscriber.

For call-response (request–reply) topics, the execution host forwards only the first reply received back to the NATS reply subject. Secondary subscribers that receive the same message but do not reply are treated as observe-only; their responses are discarded. This makes call-response safe with multiple subscribers: at most one reply is delivered, and observe-only subscribers (e.g. for logging or metrics) can co-exist on the same topic without interfering with the primary responder.

The `TopicConflict` status condition and ValidatingWebhook check are removed: sharing a topic is no longer an error.

### Tasks

- [ ] Change each function's NATS consumer group ID from the topic name to `{namespace}-{app_name}-{function_name}`; remove the topic uniqueness constraint from the operator reconciler.
- [ ] Remove `TopicConflict` from `Application` status conditions, the ValidatingWebhook handler, and all related operator logic.
- [ ] Execution host: for call-response invocations, forward only the first reply received to the NATS reply subject; discard subsequent replies from co-subscribers.
- [ ] Update e2e tests: deploy two Applications subscribing to the same topic; assert both receive the published message; assert that for a call-response topic, exactly one reply is forwarded to the caller.
- [ ] Update wp-operator README and `docs/architecture.md` to document multi-subscriber semantics and consumer group identity.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. The suite must pass with tests confirming: (1) two Applications subscribed to the same topic both receive a published message; (2) for a call-response topic, exactly one reply is returned to the caller when multiple subscribers are present; (3) applying an `Application` with a topic name that is already used by another Application is admitted without error.

---

## Phase 16: Web UI

### Design

Add a browser-based UI that gives platform operators and developers a live view of the running platform. The UI is served as a single-page application by a new `wp-ui` component, backed by a lightweight Go API server that aggregates data from the Kubernetes API, Loki, and Prometheus.

**Graph view (default):** renders all `Application` CRDs cluster-wide as a dependency graph — functions and middlewares are nodes; middleware chain membership and topic subscriber relationships are directed edges. Each node is colour-coded by readiness (`Ready: True` → green, degraded → amber, unknown/error → red). Hovering a node shows a tooltip with the function name, trigger type, and active feature flags (KV, SQL). This is the landing page.

**Function / middleware detail panel:** clicking any node opens a right-hand panel with three tabs:

- **Status:** current `Ready` condition, all other status conditions (e.g. `MetricConflict`, infrastructure state), and image metadata — module ref, OCI digest (when available from Phase OCI Digest Pinning), and module size reported by the module-cache.
- **Logs:** a streaming Loki log view scoped to the selected function (`{namespace, app, function}` label set), expressed as a LogQL query. The UI sends the query to the `wp-ui` API server, which proxies it to the configured Loki HTTP endpoint.
- **Metrics:** a PromQL-backed sparkline and table for the function's invocation count, error rate, and p99 latency. The UI sends the query to the `wp-ui` API server, which proxies it to the configured Prometheus HTTP endpoint.

**Architecture:**
- `components/wp-ui/` — a Go binary with two responsibilities: (1) serve the compiled SPA from an embedded `embed.FS`; (2) expose a JSON REST API (`/api/applications`, `/api/logs`, `/api/metrics`) that the SPA calls. The API server reads `Application` CRDs via an in-cluster Kubernetes client (same RBAC as wp-operator, read-only); it proxies Loki and Prometheus queries server-side to avoid CORS issues and to keep credentials out of the browser.
- The SPA is built from a `web/` directory inside `components/wp-ui/` using a standard Node.js build step (Vite or similar). The build output is embedded at compile time via `go:embed`.
- Helm chart: add `wp-ui` Deployment, Service, and a read-only `ClusterRole` / `ClusterRoleBinding` for `applications` resources. Loki and Prometheus endpoints are configurable via Helm values.

### Tasks

- [ ] Scaffold `components/wp-ui/`: Go module, `main.go`, `web/` SPA skeleton (Vite + TypeScript), Dockerfile, and Tiltfile resource.
- [ ] Implement the `/api/applications` endpoint: list all `Application` CRDs cluster-wide, returning name, namespace, status conditions, trigger types, and feature flags (`spec.sql`, `spec.kv`).
- [ ] Implement the graph view SPA page: render functions and middlewares as nodes, middleware chain and topic edges as directed edges; colour-code by readiness.
- [ ] Show status and feature flags (KV, SQL) as node annotations in the graph.
- [ ] Implement the detail panel — Status tab: display `Ready` and all other status conditions plus image metadata from the Application spec.
- [ ] Implement the `/api/logs` proxy endpoint: accept `namespace`, `app`, `function`, and time-range parameters; construct and forward a LogQL query to the configured Loki endpoint; stream NDJSON responses to the browser.
- [ ] Implement the Logs tab in the detail panel: call `/api/logs` and render a live-scrolling log view.
- [ ] Implement the `/api/metrics` proxy endpoint: accept a PromQL expression and time-range; forward to the configured Prometheus endpoint; return JSON series data.
- [ ] Implement the Metrics tab: display invocation count, error rate, and p99 latency sparklines and summary table via `/api/metrics`.
- [ ] Add `wp-ui` Deployment, Service, `ClusterRole`, and `ClusterRoleBinding` to the Helm chart; add `wpUi.lokiEndpoint` and `wpUi.prometheusEndpoint` Helm values.
- [ ] Update `docs/architecture.md` to document the `wp-ui` component, its data sources, and the server-side proxy pattern.
- [ ] Add e2e test: deploy an Application, assert that `/api/applications` returns it with correct status; assert that the UI root (`/`) returns HTTP 200.
- [ ] Trigger `e2e-tests` via the Tilt MCP server and confirm it passes.

### Verification

Trigger the `e2e-tests` resource via the Tilt MCP server. The suite must pass with tests confirming: (1) the `wp-ui` service returns HTTP 200 for the SPA root; (2) `/api/applications` returns all deployed Applications with correct names, namespaces, status conditions, and feature flags; (3) the Loki and Prometheus proxy endpoints return valid responses when the backing services are available; (4) all existing e2e tests continue to pass.

---

## Future Work: OCI Digest Pinning

The operator currently copies `spec.functions[].module` verbatim into `FunctionConfig.module_ref`. When a mutable tag (e.g. `:latest`) is used, different replicas may resolve different digests, updates are not detected on image push, and there is no audit trail of which digest is running.

### Tasks

- [ ] Operator resolves mutable OCI tags to immutable `sha256:` digests via the registry before pushing config to execution hosts.
- [ ] Record the resolved digest in Application status for observability.
- [ ] Re-resolve periodically (or on webhook) to detect upstream image changes and trigger a config update.
- [ ] Ensure all replicas converge on the same digest for a given generation.

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