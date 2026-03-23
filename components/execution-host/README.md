# execution-host

The runtime engine of the wasm platform: accepts incoming requests and loads and invokes WASM modules based on its current configuration.

## Module Loading

When the wp-operator pushes a new application config, the execution host:

1. Queries the module cache for a precompiled artifact keyed by module digest, CPU architecture, and Wasmtime version.
2. If found, loads the cached `.cwasm` artifact directly (no compilation required).
3. If not found, pulls the raw `.wasm` OCI artifact from the registry, AOT-compiles it using the local Wasmtime engine, pushes the compiled artifact back to the module cache, and then loads it.

## TODO

1. explicitly pull in the hello-world WASM module and hard-code the request handler to call it as a POC test.