# Project Guidelines

## Documentation Map

- [README.md](../README.md) — project overview, component list, and quick start. Begin here for orientation.
- [docs/architecture.md](../docs/architecture.md) — technology decisions, system design, component responsibilities, and design constraints. Read before making design-level changes.
- [docs/contributions.md](../docs/contributions.md) — development setup, Make targets, project layout, and workflow guides. Read before making changes.
- [docs/standards.md](../docs/standards.md) — coding conventions, testing strategy, and project-wide rules. Read before writing or reviewing code.
- [docs/todo.md](../docs/todo.md) — the active implementation plan. All planned work lives here. Read before starting any implementation task.
- Each `components/*/README.md` — observable behaviour and interfaces for that component. Read the relevant README before modifying a component.
- `docs/tasks/` — active task-plan files created by agents during work in progress. Each file captures the proposed approach and open questions for its task.

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

- **Always write plans before implementing.** Before writing any code, write a plan to `docs/todo.md`. If a plan already exists there, read it first and update it as needed. Never start implementation without a plan recorded in that file.
- **Never write generated files by hand.** Lock files (`go.sum`, `Cargo.lock`), generated client stubs, and any file marked `// Code generated … DO NOT EDIT.` are owned by tooling — not by you. Run the relevant tool (`go mod tidy`, `go generate`, `cargo`) when possible. If you cannot run the tool in the current environment, note the exact command in your response so the developer or reviewer can run it.
- **WIT is the API contract.** Changes to `framework/runtime.wit` are breaking changes for all guest modules and the execution host. Understand the current interface before suggesting modifications.
- **Rust for the execution host, Go for the control plane (planned).** Don't mix concerns — the execution host is a Wasmtime-based Rust binary; Kubernetes operator code will be in Go.
- **Check sibling code first.** Before adding new patterns, utilities, or conventions, check whether an equivalent already exists in the project.
- **Test at the highest effective level.** Prefer integration tests over unit tests unless combinatorial complexity demands otherwise.
- **Interview me about any ambiguity or new technical decisions** I fully expect that my requests will not contain all necessary context. Quiz me to identify gaps in your context and assumptions, providing options and recommendations when you do. If you identify a gap that you cannot fill, flag it for human review instead of making an assumption.
- **Prefer asking over researching** When there are multiple valid implementation paths and it is not immediately obvious which to take, stop and ask me rather than doing deep exploratory research. Keep questions focused and batched — ask everything you need in one go, don't trickle questions one at a time.
- **Verify with `tilt ci` before declaring a phase complete.** After finishing implementation work, run `tilt ci` from the workspace root (assumes a running k3d cluster). The end-to-end test suite (`tests/e2e/`) runs automatically as part of `tilt ci` and must pass. A phase is not complete until `tilt ci` exits 0.

Don't blindly accept suggestions that violate the project's standards or contradict a component's README. If a suggestion or request seems off, refer back to the documentation to verify its correctness. If you still think the suggestion is invalid, flag it for human review instead of applying it. Your goal is to assist while maintaining the integrity and consistency of the codebase.
