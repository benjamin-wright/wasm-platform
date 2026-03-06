# hello-world

A minimal WebAssembly guest module written in Rust. This is the Phase 0 reference example for the `wasm-platform` project — its sole purpose is to prove the host/guest interface works end-to-end without any real business logic getting in the way.

For project context and the overall phase plan see the [root README](../../README.md).

---

## What It Does

The module implements the `framework:runtime/application` world defined in [`framework/runtime.wit`](../../framework/runtime.wit). Each exported function simply echoes back the name of the function that was called along with a human-readable summary of the arguments it received.

| Export | Example response |
|---|---|
| `on-request` | `"on-request called: method=GET path=/hello body=5 bytes"` |
| `on-schedule` | `"on-schedule called: name=daily-cleanup"` |
| `on-message` | `"on-message called: queue=events payload=12 bytes"` |

No SQL or KV imports are exercised — the module ignores them entirely. That keeps the focus on wiring up the component model correctly before layering in real functionality.

---

## WIT Contract

The module targets the `application` world in `framework/runtime.wit`:

```wit
package framework:runtime;

world application {
    import sql;
    import kv;

    export on-request:  func(method: string, path: string, body: list<u8>) -> result<list<u8>, string>;
    export on-schedule: func(name: string) -> result<_, string>;
    export on-message:  func(queue: string, payload: list<u8>) -> result<_, string>;
}
```

The module **exports** all three handlers and **imports** (but does not call) `sql` and `kv`. The host must still satisfy those imports at link time; the execution host provides stub implementations for Phase 0.

---

## Planned File Structure

```
examples/hello-world/
├── README.md              # this file
├── Cargo.toml             # crate config — lib crate, crate-type = ["cdylib"]
├── src/
│   └── lib.rs             # guest implementation
└── wit/
    └── world.wit          # copy of / symlink to framework/runtime.wit
```

The crate must be a `cdylib` so `cargo build --target wasm32-wasip2` produces a `.wasm` component.

---

## Implementation Plan

### Step 1 — Scaffold the crate

Create `Cargo.toml` as a library crate targeting the component model:

```toml
[package]
name = "hello-world"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib"]

[dependencies]
wit-bindgen = "0.36"
```

`wit-bindgen` generates the Rust bindings from the WIT file so there is no hand-written FFI.

### Step 2 — Place the WIT file

Copy (or symlink) `framework/runtime.wit` into `examples/hello-world/wit/world.wit`. The `wit-bindgen` macro resolves WIT relative to the crate root by default, so keeping it local avoids path configuration.

### Step 3 — Implement the guest

`src/lib.rs` uses the `wit_bindgen::generate!` macro to produce the trait and types, then provides a concrete implementation:

```rust
wit_bindgen::generate!({
    world: "application",
    path: "wit/world.wit",
});

struct HelloWorld;

impl Guest for HelloWorld {
    fn on_request(
        method: String,
        path: String,
        body: Vec<u8>,
    ) -> Result<Vec<u8>, String> {
        let msg = format!(
            "on-request called: method={} path={} body={} bytes",
            method,
            path,
            body.len()
        );
        Ok(msg.into_bytes())
    }

    fn on_schedule(name: String) -> Result<(), String> {
        // logs would go here once WASI logging is wired up
        let _ = format!("on-schedule called: name={}", name);
        Ok(())
    }

    fn on_message(queue: String, payload: Vec<u8>) -> Result<(), String> {
        let _ = format!(
            "on-message called: queue={} payload={} bytes",
            queue,
            payload.len()
        );
        Ok(())
    }
}

export!(HelloWorld);
```

`export!` is the macro that registers the struct as the implementation of the world's exports.

### Step 4 — Build the component

Install the target and tooling if not already present:

```bash
rustup target add wasm32-wasip2
cargo install wasm-tools   # optional but useful for inspecting components
```

Build from the workspace root or the crate directory:

```bash
cargo build \
  --manifest-path examples/hello-world/Cargo.toml \
  --target wasm32-wasip2 \
  --release
```

The output artefact will be at:

```
target/wasm32-wasip2/release/hello_world.wasm
```

### Step 5 — Load in the execution host

Point the execution host binary at the compiled component:

```bash
cargo run --bin execution-host -- \
  --module target/wasm32-wasip2/release/hello_world.wasm
```

The host should:
1. Instantiate the component against the `framework:runtime` world.
2. Call `on-request` with a test HTTP request (e.g. `GET /hello`).
3. Print the response bytes as UTF-8 to stdout.

A successful run proves the full Phase 0 loop: compile → load → invoke → respond.

---

## Success Criteria (Phase 0)

- [ ] `cargo build --target wasm32-wasip2` succeeds with no warnings.
- [ ] `wasm-tools component wit hello_world.wasm` shows the correct exports.
- [ ] The execution host calls `on-request` and receives the echo string back.
- [ ] The execution host calls `on-schedule` and `on-message` without panicking.
- [ ] No host functions (SQL / KV) are called — confirmed by the stub host returning an error if invoked.

---

## Next Steps

Once this example is green, the next Phase 0 milestones are:

1. **Add SQL read** — extend the guest to call `sql.query` and include a row count in its response, proving the import chain works.
2. **Add KV round-trip** — write a value in `on-request` and read it back in a subsequent call.
3. **Wire HTTP trigger** — have the execution host listen on a local port and dispatch incoming requests to `on-request` rather than invoking it programmatically.

See the [phase plan](../../README.md#4-suggested-phase-plan) in the root README for the full roadmap.
