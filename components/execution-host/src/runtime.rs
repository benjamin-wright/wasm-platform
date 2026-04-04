use anyhow::Result;
use wasmtime::{
    Engine, Store,
    component::{Component, Linker, ResourceTable, bindgen},
};
use wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView};

// Generate typed bindings for the `application` world defined in runtime.wit.
// This gives us a strongly-typed `call_on_message` method and will also
// generate `add_to_linker` helpers for the `sql`, `kv`, and `messaging`
// imports once we implement those host-side traits.
bindgen!({
    world: "application",
    path: "../../framework/runtime.wit",
});

// ── Host state ────────────────────────────────────────────────────────────────
// One instance per request/call.  Adding kv or sql support later is a one-liner
// per field; the WasiView impl below will not need to change.

struct HostState {
    wasi: WasiCtx,
    table: ResourceTable,
    // kv:  KvState,   // TODO: add when wiring up the kv host implementation
    // sql: SqlState,  // TODO: add when wiring up the sql host implementation
}

impl WasiView for HostState {
    fn ctx(&mut self) -> WasiCtxView<'_> {
        WasiCtxView {
            ctx: &mut self.wasi,
            table: &mut self.table,
        }
    }
}

// ── Shared (process-wide) state ───────────────────────────────────────────────
// The Engine and Linker are expensive to create and safe to share across
// threads.  Building the Linker once here avoids repeating add_to_linker_sync
// on every message invocation.

pub struct RuntimeState {
    pub engine: Engine,
    linker: Linker<HostState>,
}

impl RuntimeState {
    pub fn new(engine: Engine) -> Result<Self> {
        let mut linker: Linker<HostState> = Linker::new(&engine);
        // Add WASI host functions.  When kv/sql/messaging are ready, call their
        // equivalent `add_to_linker` functions here.
        wasmtime_wasi::p2::add_to_linker_sync(&mut linker)?;
        Ok(Self { engine, linker })
    }
}

// ── WASM invocation ───────────────────────────────────────────────────────────

pub fn invoke_on_message(
    state: &RuntimeState,
    component: &Component,
    payload: &[u8],
) -> Result<Option<Vec<u8>>> {
    let host_state = HostState {
        wasi: WasiCtxBuilder::new().inherit_stderr().build(),
        table: ResourceTable::new(),
    };
    let mut store = Store::new(&state.engine, host_state);

    let app = Application::instantiate(&mut store, component, &state.linker)?;

    let result = app.call_on_message(&mut store, payload)?;

    result.map_err(|msg| anyhow::anyhow!("component returned error: {msg}"))
}
