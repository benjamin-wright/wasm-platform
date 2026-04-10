# Project Guidelines

## Documentation Map

- [README.md](../README.md) — project overview, component list, and quick start. Begin here for orientation.
- [docs/architecture.md](../docs/architecture.md) — technology decisions, system design, component responsibilities, and design constraints. Read before making design-level changes.
- [docs/contributions.md](../docs/contributions.md) — development setup, Make targets, project layout, and workflow guides. Read before making changes.
- [docs/standards.md](../docs/standards.md) — coding conventions, testing strategy, and project-wide rules. Read before writing or reviewing code.
- [docs/todo.md](../docs/todo.md) — the active implementation plan. All planned work lives here. Read before starting any implementation task.
- Each `components/*/README.md` — observable behaviour and interfaces for that component. Read the relevant README before modifying a component.

## Navigation

- Compilable components live under `components/`; each has its own `README.md`.
- Example guest modules live under `examples/`; each has its own `README.md`.
- The WIT interface definition is at `framework/runtime.wit` — this is the platform's API surface.
- Helm charts live under `helm/`.
- Shared Rust libraries (when added) will live under `lib/`.
- Build and cluster targets are in the root `Makefile`.

## Agent Posture

**Gather context freely. Reason proactively. Decide nothing without approval.**

- Read broadly — search the codebase, read documentation, use the context7 MCP server, run tests. Never guess when you can look.
- When you identify multiple valid approaches, present them with trade-offs and a recommendation. Do not pick one silently.
- Question directives that conflict with existing code, standards, or architecture — cite the specific conflict and ask for resolution.
- Suggest simpler or more effective alternatives when you see them, even if not asked.
- Never make design decisions, deviate from `docs/todo.md`, or take irreversible actions without explicit approval.
- Never write generated files by hand (`go.sum`, `Cargo.lock`, `// Code generated … DO NOT EDIT.`). Run the tool or note the exact command.

## Context Before Action

Before changing code, read the relevant sources — do not rely on memory or assumptions.

| Change type | Required reading |
|---|---|
| Any implementation | `docs/todo.md`, `docs/standards.md` |
| A specific component | `components/<name>/README.md` |
| Design or structural | `docs/architecture.md` |
| Build, deploy, or workflow | `docs/contributions.md` |
| WIT interface | `framework/runtime.wit`, `docs/architecture.md`, all affected component READMEs |
