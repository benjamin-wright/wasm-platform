# Implementation Plan

Remaining work for the execution-host Helm chart and container image, and the project-wide standards that cover these concerns for future components.

---

## Completed

- `components/execution-host/src/main.rs` — `WASM_MODULE_PATH` env var; dispatches to `Component::deserialize_file` for `.cwasm` or `Component::from_file` for `.wasm` (preserves `make run` workflow).
- `components/execution-host/src/bin/precompile.rs` — AOT compilation binary using the same `Engine` configuration as the runtime. Called in the Docker build to produce the `.cwasm` artifact.
- `components/execution-host/Dockerfile` — four-stage build: cargo-chef installer → dependency planner → binary builder → AOT compiler → `distroless/cc-debian12:nonroot` runtime. WASM guest passed in via `--build-context wasm=...`.
- `.dockerignore` — excludes `target/` and `.git/` from the build context.
- `Makefile` — `IMAGE_TAG` variable; `docker-build` target pre-builds the WASM guest then passes it as the named build context.

---

## Remaining

### 1. Helm chart — `helm/execution-host/`

Create the following files:

#### `helm/execution-host/Chart.yaml`

```yaml
apiVersion: v2
name: execution-host
description: Wasmtime-based WASM execution host for wasm-platform.
type: application
version: 0.1.0
appVersion: latest
```

#### `helm/execution-host/values.yaml`

```yaml
image:
  repository: wasm-platform-registry.localhost:5001/execution-host
  tag: latest
  pullPolicy: IfNotPresent

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi

replicaCount: 1
```

#### `helm/execution-host/templates/_helpers.tpl`

Define a single `execution-host.labels` helper emitting the standard `app.kubernetes.io/*` label set:

```
app.kubernetes.io/name: execution-host
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
```

#### `helm/execution-host/templates/deployment.yaml`

- `metadata.name: {{ .Release.Name }}`
- Labels from `execution-host.labels` helper on both the Deployment and the pod template.
- `spec.replicas: {{ .Values.replicaCount }}`
- Pod security context:
  ```yaml
  runAsNonRoot: true
  seccompProfile:
    type: RuntimeDefault
  ```
- Container security context:
  ```yaml
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop: ["ALL"]
  ```
- Image: `{{ .Values.image.repository }}:{{ .Values.image.tag }}`, `pullPolicy: {{ .Values.image.pullPolicy }}`
- `env` — set `WASM_MODULE_PATH` to `/opt/wasm/hello_world.cwasm` (matches the image default; explicit here so it's visible in the manifest).
- Container port 3000 named `http`.
- Liveness probe: `httpGet /healthz :3000`, `initialDelaySeconds: 5`, `periodSeconds: 10`.
- Readiness probe: same path, same port.
- `resources: {{ toYaml .Values.resources | nindent 10 }}`
- No namespace hardcoded — use `.Release.Namespace` if a namespace reference is needed.

#### `helm/execution-host/templates/service.yaml`

- `metadata.name: {{ .Release.Name }}`
- `type: ClusterIP`
- Port 80 named `http` → targetPort 3000.
- Selector: `app.kubernetes.io/instance: {{ .Release.Name }}`

---

### 2. Standards additions — `docs/standards.md`

Add two sections.

#### Under the existing `## Kubernetes` heading, replace the stub `### Helm Charts` bullet list with:

```markdown
### Helm Charts

- Every component that runs in Kubernetes gets a chart at `helm/<component-name>/`.
- `Chart.yaml` `name` must match the component directory name under `components/`.
- `appVersion` must match the container image tag deployed by that chart version.
- `values.yaml` exposes exactly three concerns: `image` (repository, tag, pullPolicy), `resources` (requests and limits), and `replicaCount`. Do not add parameters for things that do not vary across environments.
- All resource `metadata.name` values use `{{ .Release.Name }}` — no fullname helper, no nameOverride.
- All resources carry the standard label set from `_helpers.tpl`: `app.kubernetes.io/name`, `app.kubernetes.io/instance`, `app.kubernetes.io/version`, `app.kubernetes.io/managed-by`.
- Every HTTP service must define liveness and readiness probes against `/healthz`.
- Pod security context must set `runAsNonRoot: true` and `seccompProfile.type: RuntimeDefault`.
- Container security context must set `allowPrivilegeEscalation: false`, `readOnlyRootFilesystem: true`, and `capabilities.drop: ["ALL"]`.
- No hardcoded namespaces — use `.Release.Namespace` throughout.
```

#### Add a new `## Container Images` section (e.g. after `## WebAssembly`):

```markdown
## Container Images

- Every component under `components/` that produces a binary must include a `Dockerfile` at `components/<name>/Dockerfile`.
- The build context is always the **repo root** so the full workspace is available for cargo-chef dependency resolution.
- Builds must use `cargo-chef` for dependency caching: a dedicated `planner` stage runs `cargo chef prepare`, a `builder` stage runs `cargo chef cook` before copying source.
- The builder base image version must match the `rust-version` declared in the component's `Cargo.toml`.
- WASM guest modules are passed into the image build as a named build context (`--build-context wasm=<path>`) — they are not compiled inside the Dockerfile. The Makefile target is responsible for building the `.wasm` before invoking `docker build`.
- WASM modules must be AOT-compiled to a `.cwasm` artifact in a dedicated build stage using the `precompile` binary before being copied to the runtime image. This eliminates JIT compilation at startup.
- The runtime base image must be `gcr.io/distroless/cc-debian12:nonroot`. Use `distroless/base-debian12:nonroot` only if a verified `ldd` confirms no `libgcc_s` dependency — document the finding in the Dockerfile.
- The final image must contain no shell, package manager, or build tooling.
- `readOnlyRootFilesystem: true` must be achievable — binaries must not write to the filesystem at runtime.
- The `EXPOSE` instruction must declare exactly the port(s) the process listens on.
```

---

### 3. Documentation updates — `docs/contributions.md`

Three small additions:

#### Make Targets table — add one row:

| `make docker-build` | Build the execution-host container image and tag it for the local registry |

#### Project Layout — replace `helm/` placeholder line:

```
├── helm/
│   └── execution-host/         # Helm chart for the execution host
```

And add `.dockerignore` at the root level entry.

#### "Adding a New Component" workflow — append two steps:

5. Add a `Dockerfile` at `components/<name>/Dockerfile` following the container image standards.
6. Add a Helm chart at `helm/<name>/` following the Helm chart standards.

---

## Implementation Order

1. Helm chart files (`Chart.yaml`, `values.yaml`, `_helpers.tpl`, `deployment.yaml`, `service.yaml`)
2. Standards additions to `docs/standards.md`
3. Documentation updates to `docs/contributions.md`
