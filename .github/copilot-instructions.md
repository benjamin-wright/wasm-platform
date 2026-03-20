# Project Guidelines

## Documentation Map

- [README.md](../README.md) — project overview, component list, and quick start. Begin here for orientation.
- [docs/architecture.md](../docs/architecture.md) — technology decisions, system design, component responsibilities, and design constraints. Read before making design-level changes.
- [docs/contributions.md](../docs/contributions.md) — development setup, Make targets, project layout, and workflow guides. Read before making changes.
- [docs/standards.md](../docs/standards.md) — coding conventions, testing strategy, and project-wide rules. Read before writing or reviewing code.
- Each `components/*/README.md` — observable behaviour and interfaces for that component. Read the relevant README before modifying a component.

## Navigation

- Compilable components live under `components/`; each has its own `README.md`.
- Example guest modules live under `examples/`; each has its own `README.md`.
- The WIT interface definition is at `framework/runtime.wit` — this is the platform's API surface.
- Helm charts live under `helm/`.
- Shared Rust libraries (when added) will live under `lib/`.
- Build and cluster targets are in the root `Makefile`.

## AI Agent Instructions

You are an AI agent assisting with code generation and review in this repository. Use the documentation map above to find relevant information about project structure, coding standards, and component behaviour. Always check the `README.md` for the component you're working on to understand its observable behaviour and interfaces. Follow the coding conventions in `docs/standards.md` to ensure consistency across the project.

Key points:

- **WIT is the API contract.** Changes to `framework/runtime.wit` are breaking changes for all guest modules and the execution host. Understand the current interface before suggesting modifications.
- **Rust for the execution host, Go for the control plane (planned).** Don't mix concerns — the execution host is a Wasmtime-based Rust binary; Kubernetes operator code will be in Go.
- **Check sibling code first.** Before adding new patterns, utilities, or conventions, check whether an equivalent already exists in the project.
- **Test at the highest effective level.** Prefer integration tests over unit tests unless combinatorial complexity demands otherwise.

Don't blindly accept suggestions that violate the project's standards or contradict a component's README. If a suggestion or request seems off, refer back to the documentation to verify its correctness. If you still think the suggestion is invalid, flag it for human review instead of applying it. Your goal is to assist while maintaining the integrity and consistency of the codebase.
