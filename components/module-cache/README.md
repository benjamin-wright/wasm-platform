# module-cache

A sidecar container running alongside each execution host pod. Watches `Application` CRDs, pulls WebAssembly modules from the OCI registry, AOT-compiles them via Wasmtime, and writes the result to a PVC shared with the execution host container. The execution host reads compiled artifacts directly from the shared volume without network overhead.

## Cache Entries

Each entry is keyed by the OCI digest of the source module and contains a single artifact:

| Artifact | Description |
|----------|-------------|
| `.cwasm` | Wasmtime AOT-compiled native module serialised to disk |

## Interfaces

### Writers

The module-cache sidecar populates the shared PVC:

1. Watches `Application` CRDs for new or updated module digests.
2. Pulls the `.wasm` module from the OCI registry by digest.
3. AOT-compiles it via Wasmtime.
4. Writes the `.cwasm` artifact to the shared volume, keyed by digest.

### Readers

The execution host reads from the shared PVC at invocation time:

1. Resolves the application's current module digest.
2. Loads the `.cwasm` artifact, memory-mapped, for instantiation.

## Deployment

The module-cache sidecar and execution host container run in the same pod within the execution host StatefulSet. They share the pod's PVC via a mounted volume. The PVC persists across pod restarts; compiled artifacts do not need to be recompiled after a rolling update.

## TODO

1. Define the on-volume directory layout and file naming convention.
2. Decide cache eviction policy for modules no longer referenced by any `Application` CRD.
3. Specify PVC size limits and storage class requirements.