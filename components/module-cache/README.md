# Centralized Cache Design

This module implements a centralized cache design keyed by digest, architecture, and Wasmtime version.

## Overview
The cache is a shared storage service for precompiled WASM modules. It does not perform compilation itself — that responsibility belongs to the execution host. The cache stores and retrieves AOT-compiled artifacts, minimizing redundant compilation across the fleet.

## Cache Key Structure
- **Digest**: A cryptographic hash representing the contents of the module.
- **Architecture**: Defines the target architecture for which the module has been compiled (e.g., x86_64, arm).
- **Wasmtime Version**: Specifies the version of the Wasmtime runtime used for compilation, ensuring compatibility with the execution environment.

## Workflow for Precompiled Modules

The execution host is the active participant in this workflow; the cache is purely a storage layer.

1. **Cache Hit**:
   - The execution host queries the cache using the digest, architecture, and Wasmtime version as the key.
   - If an entry is found, the precompiled artifact is returned to the execution host for immediate use.

2. **Cache Miss**:
   - If no entry is found, the execution host pulls the raw `.wasm` OCI artifact from the registry.
   - The execution host AOT-compiles the artifact using its local Wasmtime engine.
   - The resulting compiled artifact is pushed back to the cache, keyed by digest, architecture, and Wasmtime version, so subsequent requests from any execution host can reuse it.

This design keeps the cache simple and stateless, while allowing any execution host to warm the cache on behalf of the fleet.