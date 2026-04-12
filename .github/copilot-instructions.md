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

## Cloud Agent Environment

Cloud agents (GitHub Copilot coding agent) run in an ephemeral GitHub Actions environment. Several things work differently from a local developer machine. Read this section before running `tilt up` in a cloud agent session.

### What `.github/workflows/copilot-setup-steps.yml` does

This workflow runs automatically before every agent session and handles:

- Installing **k3d**, **Tilt**, and **oras**.
- Creating the k3d cluster **without a load balancer** (`--no-lb`) and with Traefik, servicelb, and metrics-server disabled — port 80 cannot be bound to in the runner environment, and disabling unused system components reduces image-pull requirements and eliminates a pod-networking bug triggered by those components.
- Pre-pulling the k3s system images from the host and importing them with `k3d image import` — the DNS resolver inside k3d containers is unreliable on Azure-hosted runners (transient `EAGAIN` failures against `registry-1.docker.io`), so images must be seeded from the host Docker daemon.
- Adding the registry container's IP to the k3d server container's `/etc/hosts` — `wasm-platform-registry.localhost` is not otherwise resolvable from inside the k3d container.
- Pre-warming the Docker layer cache for the `workspace-deps` build stage.
- Starting Tilt in the background so builds begin during the agent startup window.

If the setup steps ran successfully, the cluster is ready and Tilt is already running. Skip straight to **Fixing Tilt MCP connectivity** below.

### Fixing Tilt MCP connectivity

The Tilt MCP server runs in a Docker container and reaches Tilt at `host.docker.internal:10350`. On Azure-hosted runners `host.docker.internal` does not resolve inside that container. Fix it once per session before using any Tilt MCP tools:

```bash
BRIDGE_IP=$(ip addr show docker0 | grep 'inet ' | awk '{print $2}' | cut -d/ -f1)
TILT_MCP=$(docker ps -q --filter ancestor=benwright/tilt-mcp:v0.0.4)
docker exec -u root "$TILT_MCP" sh -c "echo '${BRIDGE_IP} host.docker.internal' >> /etc/hosts"
```

Verify with `tilt-get-session` — if it returns the session state, connectivity is working.

### Starting Tilt manually (if setup steps did not run)

If this is the first session after a fresh clone, before `copilot-setup-steps.yml` is on the default branch, bootstrap manually:

```bash
# 1. Create cluster (no LB, minimal system services)
k3d cluster create wasm-platform \
  --registry-create wasm-platform-registry.localhost:0.0.0.0:5001 \
  --kubeconfig-update-default=false \
  --no-lb \
  --k3s-arg "--disable=traefik,servicelb,metrics-server@server:*" \
  --wait
mkdir -p ~/.scratch
k3d kubeconfig get wasm-platform > ~/.scratch/wasm-platform.yaml
export KUBECONFIG=~/.scratch/wasm-platform.yaml

# 2. Pre-pull and import k3s system images (Docker Hub DNS unreliable inside k3d)
docker pull rancher/mirrored-pause:3.6 &
docker pull rancher/mirrored-coredns-coredns:1.12.0 &
docker pull rancher/local-path-provisioner:v0.0.30 &
docker pull benwright/db-operator:v1.0.8 &
wait
k3d image import \
  rancher/mirrored-pause:3.6 \
  rancher/mirrored-coredns-coredns:1.12.0 \
  rancher/local-path-provisioner:v0.0.30 \
  benwright/db-operator:v1.0.8 \
  --cluster wasm-platform

# 3. Fix registry hostname in k3d server container
REGISTRY_IP=$(docker inspect \
  --format '{{range $k,$v := .NetworkSettings.Networks}}{{if eq $k "k3d-wasm-platform"}}{{$v.IPAddress}}{{end}}{{end}}' \
  wasm-platform-registry.localhost)
docker exec -u root k3d-wasm-platform-server-0 \
  sh -c "echo '${REGISTRY_IP} wasm-platform-registry.localhost' >> /etc/hosts"

# 4. Start Tilt
tilt up --host 0.0.0.0 --port 10350 > /tmp/tilt.log 2>&1 &
```

### Checking build status

Use `tilt-wait-for-build` (with `timeoutSeconds: 900`) to block until all builds complete rather than polling. The `workspace-deps` build stage takes 5–10 minutes on a cold Docker cache; the setup steps pre-warm it, but subsequent Rust component builds still add several minutes each.

### Known limitations in cloud sessions

| Limitation | Detail |
|---|---|
| No load balancer | `make cluster-up` uses `-p 80:80@loadbalancer` which fails in CI. Use the manual bootstrap above or rely on setup steps. |
| E2e tests use port 3000 | The `e2e-tests` Tilt resource hits `http://localhost:3000/hello` via Tilt's built-in port-forward on the gateway resource — no ingress controller required. |
| System image list is pinned | The k3s image versions in `copilot-setup-steps.yml` are pinned to k3s v1.31. If the k3s image in k3d changes, update the versions to match (`kubectl get pods -A -o jsonpath='{...image...}'` after cluster creation). |
| `host.docker.internal` in tilt-mcp | Must be fixed manually each session (see above). |
