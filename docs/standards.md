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
- Follow the conventions established in sibling components. Consistency takes priority over local preference.
- Prefer library-provided functions over hand-rolled logic for serialisation, encoding, etc.

### Code Clarity

- Names (functions, variables, types) must be descriptive enough to make their purpose obvious without a comment.
- Comments must add information the code cannot express — explain *why*, not *what*. Never write a comment that just restates the line it sits next to.
- Prefer fewer, meaningful comments over many redundant ones.

### Single Responsibility Principle

- Code must be well composed, with clear responsibilities for each component. This applies both in terms of subject matter (separate components for separate concerns) and in terms of clients (how something is done) and orchestrators (when something is done).
- Responsibility boundaries must be structural — enforced by distinct types or modules — not cosmetic.

### External Dependency Ownership

All interaction with an external system (database, message broker, HTTP service) must be encapsulated in a single module behind an exported interface. Other modules depend on the interface, never on the external system directly. This ensures that consumers can be tested with fakes and that external-system concerns live in one place.

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

## WebAssembly


### Guest Modules

- Guest modules must target `wasm32-wasip2` and produce a `cdylib`.
- Guests must not depend on host implementation details — they interact only through the WIT-defined imports.
- Keep guest dependencies minimal to reduce compiled module size and cold-start time.

### Host Functions

- Host function implementations are scoped per invocation — each call gets its own `Store` and `HostState`.
- Host functions must enforce capability boundaries: a module should only access the databases and queues declared in its configuration.

---

## Kubernetes

### Helm Charts

- All Kubernetes manifests live in Helm charts under `helm/`.
- Charts must be self-contained — a `helm install` should deploy the component and all its direct dependencies (service accounts, RBAC, config maps).
- Use values for anything environment-specific (image repository, tag, resource limits, replica count).

### Controllers (planned)

- Use the informer cache for all reads. Fall back to direct reads only when caching is infeasible.
- Guard status writes with a state check — skip the write if nothing has changed.
- Use deterministic names for child objects so optimistic locking detects conflicts naturally.
- After a write (Create/Update/Patch/Delete), use the object returned by the API server — do not re-read from the cache.

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
