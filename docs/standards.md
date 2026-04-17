# Project Standards

Coding conventions, testing strategy, and project-wide rules. For technology decisions and system design, see [architecture.md](architecture.md). For development setup, see [contributions.md](contributions.md).

## Documentation

Each documentation file has a single responsibility. Content must live in exactly one place — if a statement would be equally true in two files, it belongs in whichever file owns that concern. Never embed the contents of a source-of-truth file (WIT definitions, configs, source code) in documentation — reference the file instead. Embedded copies diverge silently and waste reader context.

| File | Responsibility |
|------|----------------|
| `README.md` | User-centric introduction — project summary, quick start, component list. Start here for orientation. |
| `docs/architecture.md` | Technology decisions, system design, component responsibilities, and design constraints. Read before making design-level changes. |
| `docs/standards.md` | Generalised decisions — conventions for code, tests, docs, and components. Ensures consistency. |
| `docs/contributions.md` | Development setup, build/test commands, local dev workflow. Read before making changes. |
| `.github/copilot-instructions.md` | Navigation guide for AI agents — where to find documentation and how to use it efficiently. |
| `components/*/README.md` | Observable behaviour and interfaces for a single component. No project-wide standards. |
| `examples/*/README.md` | Purpose, build instructions, and usage for a single example. |

### Component READMEs

Every compilable component under `components/` and every example under `examples/` must include a `README.md` detailing the features and interfaces that component provides.

Every line in a component README must pass these checks:

- **Concise** — no redundant phrasing, no filler words. If a shorter phrasing carries the same meaning, use it.
- **Clear** — language must be unambiguous. Describe observable behaviour, not intent.
- **No duplication** — each feature, interface, or constraint is stated in exactly one file.
- **No project-wide standards** — READMEs must not restate information already covered by `docs/standards.md`.
- **No implementation detail** — describe observable behaviour and interfaces, not internal structure.
- **No conflicts** — READMEs must not make contradictory claims across or within files.

---

## General


### Reuse Over Reinvention

Before writing anything new — utility, pattern, convention, or routine — check whether an equivalent already exists in the project or an existing dependency. If it does, use it. If it does not, create it in an appropriate shared location so others can reuse it.

- Never duplicate a helper inline across files; parameterise a shared function to cover variant contexts.
- Shared data types that define cross-component contracts (serialisation payloads, protocol types, common infrastructure) must live in a `lib/` crate so the contract is enforced at compile time, not by convention. If two components independently define the same type, extract it.
- Follow the conventions established in sibling components. Consistency takes priority over local preference.
- Prefer library-provided functions over hand-rolled logic for serialisation, encoding, etc.

### Code Clarity

- Names (functions, variables, types) must be descriptive enough to make their purpose obvious without a comment. Prefer renaming or restructuring over adding an explanatory comment.
- Doc-comments on public functions and types are encouraged — they improve IDE tooling and serve as the API contract.
- Inline comments are reserved for cases where the code's *current structure* cannot convey the reasoning — e.g. a non-obvious ordering constraint or a subtlety that naming alone cannot capture.
- Comments must not duplicate information that belongs elsewhere. Architectural decisions belong in `docs/architecture.md`, behavioural contracts belong in component READMEs, and historical context belongs in version control. Code comments are not a distributed changelog or a substitute for documentation.
- If a comment explains *what* the code does rather than *why* it does it that way, the code should be refactored until the comment is unnecessary.

### Single Responsibility Principle

- Code must be well composed, with clear responsibilities for each component. This applies both in terms of subject matter (separate components for separate concerns) and in terms of clients (how something is done) and orchestrators (when something is done).
- Responsibility boundaries must be structural — enforced by distinct types or modules — not cosmetic.

### External Dependency Ownership

All interaction with an external system (database, message broker, HTTP service) must be encapsulated in a single module behind an exported interface. Other modules depend on the interface, never on the external system directly. This ensures that consumers can be tested with fakes and that external-system concerns live in one place.

### Definition of Done

A phase or feature is complete when all of the following are true:

- Implementation tasks are finished and the `e2e-tests` Tilt resource passes (triggered via the Tilt MCP server).
- Documentation (`README.md`, architecture docs) reflects any new or changed behaviour.
- End-to-end tests (`tests/e2e/`) cover new user-facing workflows. If a feature changes what a user can observe through the platform's external interfaces, the test suite must verify it.

### End-to-End Testing

End-to-end tests live in `tests/e2e/` as a Go module with a `//go:build integration` build tag. They verify the full stack from the perspective of a platform user.

- The permanent test fixture is the hello-world Application CR (HTTP trigger, KV counter). It must remain deployed and passing at all times. Phase-specific fixtures may be added alongside it but must not break it.
- Traefik `Ingress` routes `localhost:80` to the gateway (no TLS; host is configurable via the gateway Helm chart).
- Tests are run as `go test -tags integration -count=1 -v ./...`, wired as the `e2e-tests` local resource in Tilt depending on `demo-app`. Trigger this resource and verify it passes using the Tilt MCP server.
- A phase that adds a new user-facing workflow is not complete until the e2e suite covers that workflow.

---

## Component Runtime

Standards that apply to every deployed component (Rust or Go). These ensure consistent operational behaviour across the platform.

### Graceful Shutdown

Every component must handle `SIGTERM` by stopping acceptance of new work, draining in-flight work to completion, and exiting cleanly before Kubernetes sends `SIGKILL`. A component must not rely on being force-killed to stop.

- **Rust components:** install a `tokio::signal` handler; broadcast shutdown to all subsystems; await drain.
- **Go components:** `controller-runtime` handles this via its manager lifecycle; gRPC servers use `GracefulStop()`.

### Health Probes

Every component must expose both `/healthz` (liveness) and `/readyz` (readiness) HTTP endpoints.

- **Liveness** — the process is alive and not deadlocked. Return `200` unconditionally.
- **Readiness** — all required dependencies are connected and the component can serve traffic. Return `503` until ready.

Rust components should use the shared `platform_common::health` module.

### Structured Logging

All components must use structured logging with discrete severity levels.

- **Rust:** `tracing` with `tracing-subscriber` and `EnvFilter` (controlled via `RUST_LOG`).
- **Go:** controller-runtime's structured logger.

Log entries must use key-value fields, not interpolated message strings. Components must log their name at startup (e.g. `tracing::info!("gateway starting")`) so they are identifiable in aggregated log streams.

### No Secrets in Logs

Credentials, connection strings with embedded passwords, tokens, and key material must never appear in log output at any level. Logging a struct that contains secret fields requires explicit exclusion of those fields — do not rely on the struct's default formatting.

### Prometheus Metrics

Every component must expose a `/metrics` endpoint in Prometheus exposition format for scraping. Use this to surface request counts, error rates, latency histograms, and component-specific activity indicators. The wp-operator already exposes metrics via controller-runtime; Rust components must add an equivalent endpoint.

### Fail-Fast on Invalid Configuration

All required environment variables and configuration must be validated at startup before the component enters its main loop. If configuration is missing or invalid, the component must exit immediately with a clear error message. A component must not start serving traffic with incomplete configuration.

### Bounded Concurrency and Backpressure

Components must cap in-flight work and propagate backpressure rather than buffering unboundedly. A traffic spike must result in rejection or throttling, not unbounded memory growth or cascading latency. The execution host's semaphore (`MAX_CONCURRENT_INVOCATIONS`) is the reference pattern.

### Outbound Call Timeouts

Every outbound network call (gRPC, NATS publish, HTTP, database query) must have a finite timeout. No unbounded waits. A slow dependency must not silently stall the component.

### Connection Resilience

Persistent connections (NATS, gRPC streams, database pools) must auto-reconnect on failure with exponential backoff. A transient infrastructure blip must not require a pod restart to recover.

---

## Rust

### Style

- Follow standard `rustfmt` conventions. Run `cargo fmt` before committing.
- Run `cargo clippy` and resolve all warnings.
- Use `anyhow::Result` for application-level error propagation. Use typed errors (`thiserror`) at library boundaries where callers need to match on variants.
- Prefer `?` for error propagation over explicit `match` / `unwrap`.
- Avoid `unwrap()` and `expect()` in production code paths. Use them only in tests or where the invariant is proven by construction and documented with a comment.

### Async

- All async code runs on Tokio.
- CPU-bound work (WASM execution) must be offloaded to the blocking thread pool via `tokio::task::spawn_blocking` to keep the async runtime responsive.
- Use `async` only when the function actually awaits I/O. Do not mark synchronous functions as async.

### Workspace Layout

- The repo uses a Cargo workspace defined at the root `Cargo.toml`.
- Components (deployed binaries) live under `components/`.
- Example guest modules live under `examples/`.
- Shared WIT definitions live under `framework/`.
- Shared Rust libraries (when needed) should live under a `lib/` directory and be added as workspace members.

---

## Go

### Style

- Follow standard `gofmt` conventions. Run `gofmt` (or `goimports`) before committing.
- Run `go vet` and resolve all warnings.
- Use structured errors with `fmt.Errorf` and `%w` for wrapping. Define sentinel errors with `errors.New` when callers need to match them.

### Modules and Generated Code

- Never manually write or edit `go.sum` entries or the dependency block in `go.mod`. Run `go mod tidy` to add, update, or remove them.
- Never manually write code owned by a generator. This includes:
  - CRD client stubs, informers, and listers produced by `code-generator` or `controller-gen`.
  - Any file that begins with `// Code generated … DO NOT EDIT.`
- To regenerate, run the relevant `go generate` target or the generator command documented in the component README. If you cannot run the generator in the current environment, leave a comment in the PR description with the exact command so a reviewer can run it.

---

## Component Build Ownership

Each component owns every aspect of its build that is not shared across components.

- If a generator, tool invocation, or build step applies to exactly one component, it must live in a `Makefile` at `components/<name>/Makefile`, not in the root `Makefile`.
- The root `Makefile` may provide a convenience target that delegates to component `Makefile`s (e.g. `$(MAKE) -C components/<name> generate`), but the authoritative target is always in the component.
- A component `Makefile` may expose multiple targets for distinct generation steps, but must also expose a single `generate` target that depends on all of them so callers have one entry point.

---

## WebAssembly


### Guest Modules

- Guest modules must target `wasm32-wasip2` and produce a `cdylib`.
- Guests must not depend on host implementation details — they interact only through the WIT-defined imports.
- Keep guest dependencies minimal to reduce compiled module size and cold-start time.

### Host Functions

- Host function implementations are scoped per invocation — each call gets its own `Store` and `HostState`.
- Host functions must enforce capability boundaries: a module should only access the databases and queues declared in its configuration.

---

## Container Images

- Every component under `components/` that produces a binary must include a `Dockerfile` at `components/<name>/Dockerfile`.
- The build context must be the **component directory** (`components/<name>/`). Any dependency outside the component directory must be passed as a named build context (`--build-context name=<path>`).
- Rust components that span the Cargo workspace must pass the repo root as a named build context and use `cargo-chef` for dependency caching: a dedicated `planner` stage runs `cargo chef prepare`, a `builder` stage runs `cargo chef cook` before copying source. The builder base image version must match the `rust-version` declared in the component's `Cargo.toml`.
- WASM guest modules are passed into the image build as a named build context (`--build-context wasm=<path>`) — they are not compiled inside the Dockerfile. The component `Tiltfile` builds the `.wasm` via a `local_resource` that `docker_build` depends on (`resource_deps`).
- The runtime base image must be `gcr.io/distroless/cc-debian12:nonroot` for binaries with a libc dependency. Use `gcr.io/distroless/static-debian12:nonroot` for fully static binaries (e.g. `CGO_ENABLED=0` Go builds). Document the choice in the Dockerfile.
- The final image must contain no shell, package manager, or build tooling.
- `readOnlyRootFilesystem: true` must be achievable — binaries must not write to the filesystem at runtime.
- The `EXPOSE` instruction must declare exactly the port(s) the process listens on.

---

## Kubernetes

### Helm Charts

- Every component that runs in Kubernetes gets a chart at `components/<component-name>/helm/`.
- `Chart.yaml` `name` must match the component directory name under `components/`.
- `appVersion` must match the container image tag deployed by that chart version.
- `values.yaml` exposes exactly three concerns: `image` (repository, tag, pullPolicy), `resources` (requests and limits), and `replicaCount`. Do not add parameters for things that do not vary across environments.
- All resource `metadata.name` values use `{{ .Release.Name }}` — no fullname helper, no nameOverride.
- All resources carry the standard label set from `_helpers.tpl`: `app.kubernetes.io/name`, `app.kubernetes.io/instance`, `app.kubernetes.io/version`, `app.kubernetes.io/managed-by`.
- Every HTTP service must define liveness and readiness probes against `/healthz`.
- Pod security context must set `runAsNonRoot: true` and `seccompProfile.type: RuntimeDefault`.
- Container security context must set `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, and `capabilities.drop: ["ALL"]`.
- No hardcoded namespaces — use `.Release.Namespace` throughout.

### Controllers (planned)

- Use the informer cache for all reads. Fall back to direct reads only when caching is infeasible.
- Guard status writes with a state check — skip the write if nothing has changed.
- Use deterministic names for child objects so optimistic locking detects conflicts naturally.
- After a write (Create/Update/Patch/Delete), use the object returned by the API server — do not re-read from the cache.

---

## Tilt

- Every component under `components/` must include a `Tiltfile` that defines a single public function named after its directory in `snake_case` (e.g. `execution_host` for `components/execution-host/`).
- The function owns all Tilt resources for that component: `local_resource` builds, `docker_build`, `helm_resource`, and tests.
- Every resource within the function must carry `labels=['<component-dir-name>']` so resources are grouped by component in the Tilt UI.
- WASM guest builds use `local_resource` with `deps` listing the relevant source directories so Tilt triggers rebuilds on change.
- Docker builds that require `--build-context` use `custom_build` with the full `docker build` command passed as `command`.
- Unit tests are declared as a `local_resource` with `deps` set to the component's source directory — they run automatically on source change and do not require a running cluster.
- Integration tests are declared as a `local_resource` with `resource_deps` pointing to the deployed Helm resource and `trigger_mode = TRIGGER_MODE_MANUAL` — they must be triggered explicitly in the Tilt UI.
- The root `Tiltfile` contains only `allow_k8s_contexts` and component `load` / call statements. No resources are defined at the root level.

---

## Testing

**Governing principle:** Test at the highest level that exercises the code path efficiently. Drop to a lower level only when combinatorial complexity makes the higher level impractical.

### End-to-End Tests

- Test all user workflows through the same interface the user would use.

### Integration Tests

- Deploy the application into a dedicated test namespace and test against real services (database, WASM runtime, NATS, etc.).
- Aim for the majority of test coverage here — prefer shared resources over per-test isolation.
- Access services the way a real consumer would (e.g. port-forward and connect over the network) rather than using cluster-internal shortcuts.

### Unit Tests

- Reserve for complex logic with many input permutations and minimal external dependencies.

### Test Design Rules

- Test through exported entry points. Never export a function solely for testability.
- If a component can't be unit-tested without its external dependency, refactor the dependency behind a trait that a fake can replace.
