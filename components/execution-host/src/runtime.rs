use anyhow::Result;
use serde::{Deserialize, Serialize};
use wasmtime::{
    Engine, Store,
    component::{Component, Linker, ResourceTable},
};
use wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView};

// Bindings for `world message-application` — binary payload in/out.
mod message_bindings {
    wasmtime::component::bindgen!({
        world: "message-application",
        path: "../../framework/runtime.wit",
    });
}

// Bindings for `world http-application` — typed HTTP request/response.
mod http_bindings {
    wasmtime::component::bindgen!({
        world: "http-application",
        path: "../../framework/runtime.wit",
    });
}

// ── Platform-private HTTP payload types ───────────────────────────────────────
// The gateway serialises incoming HTTP requests into this JSON format before
// publishing to the app's NATS subject.  The execution host deserialises,
// calls `on-request` with typed WIT records, and serialises the returned
// `http-response` back to JSON for the reply.  Guest modules never see JSON.

#[derive(Debug, Serialize, Deserialize)]
pub struct HttpRequestPayload {
    pub method: String,
    pub path: String,
    pub query: String,
    pub headers: Vec<(String, String)>,
    pub body: Option<Vec<u8>>,
}

#[derive(Debug, Serialize, Deserialize)]
pub struct HttpResponsePayload {
    pub status: u16,
    pub headers: Vec<(String, String)>,
    pub body: Option<Vec<u8>>,
}

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

// ── WASM invocations ──────────────────────────────────────────────────────────

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

    let app = message_bindings::MessageApplication::instantiate(
        &mut store,
        component,
        &state.linker,
    )?;

    let result = app.call_on_message(&mut store, payload)?;

    result.map_err(|msg| anyhow::anyhow!("component returned error: {msg}"))
}

pub fn invoke_on_request(
    state: &RuntimeState,
    component: &Component,
    request: HttpRequestPayload,
) -> Result<HttpResponsePayload> {
    let host_state = HostState {
        wasi: WasiCtxBuilder::new().inherit_stderr().build(),
        table: ResourceTable::new(),
    };
    let mut store = Store::new(&state.engine, host_state);

    let app = http_bindings::HttpApplication::instantiate(
        &mut store,
        component,
        &state.linker,
    )?;

    let wit_request = http_bindings::HttpRequest {
        method: request.method,
        path: request.path,
        query: request.query,
        headers: request.headers,
        body: request.body,
    };

    let result = app.call_on_request(&mut store, &wit_request)?;

    match result {
        Ok(wit_response) => Ok(HttpResponsePayload {
            status: wit_response.status,
            headers: wit_response.headers,
            body: wit_response.body,
        }),
        Err(msg) => Err(anyhow::anyhow!("component returned error: {msg}")),
    }
}
