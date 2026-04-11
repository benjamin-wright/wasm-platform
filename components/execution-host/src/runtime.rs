use anyhow::Result;
use wasmtime::{
    Engine, Store,
    component::{Component, Linker, ResourceTable},
};
use wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView};

pub use platform_common::http_types::{HttpRequestPayload, HttpResponsePayload};

// Bindings for `world message-application` — binary payload in/out.
pub(crate) mod message_bindings {
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

// ── Host state ────────────────────────────────────────────────────────────────
// One instance per request/call.

pub(crate) struct HostState {
    wasi: WasiCtx,
    table: ResourceTable,
    pub(crate) kv_prefix: String,
    pub(crate) redis_client: Option<redis::Client>,
    pub(crate) nats_client: Option<async_nats::Client>,
    pub(crate) app_name: String,
    pub(crate) app_namespace: String,
    pub(crate) function_name: String,
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
    pub redis_client: Option<redis::Client>,
}

impl RuntimeState {
    pub fn new(engine: Engine, redis_client: Option<redis::Client>) -> Result<Self> {
        let mut linker: Linker<HostState> = Linker::new(&engine);
        wasmtime_wasi::p2::add_to_linker_sync(&mut linker)?;
        message_bindings::framework::runtime::kv::add_to_linker::<HostState, wasmtime::component::HasSelf<HostState>>(
            &mut linker,
            |h: &mut HostState| h,
        )?;
        message_bindings::framework::runtime::messaging::add_to_linker::<HostState, wasmtime::component::HasSelf<HostState>>(
            &mut linker,
            |h: &mut HostState| h,
        )?;
        message_bindings::framework::runtime::log::add_to_linker::<HostState, wasmtime::component::HasSelf<HostState>>(
            &mut linker,
            |h: &mut HostState| h,
        )?;
        Ok(Self { engine, linker, redis_client })
    }
}

// ── WASM invocations ──────────────────────────────────────────────────────────

pub fn invoke_on_message(
    state: &RuntimeState,
    component: &Component,
    payload: &[u8],
    kv_prefix: String,
    nats_client: Option<async_nats::Client>,
    app_name: String,
    app_namespace: String,
    function_name: String,
) -> Result<Option<Vec<u8>>> {
    let host_state = HostState {
        wasi: WasiCtxBuilder::new().inherit_stderr().build(),
        table: ResourceTable::new(),
        kv_prefix,
        redis_client: state.redis_client.clone(),
        nats_client,
        app_name,
        app_namespace,
        function_name,
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
    kv_prefix: String,
    nats_client: Option<async_nats::Client>,
    app_name: String,
    app_namespace: String,
    function_name: String,
) -> Result<HttpResponsePayload> {
    let host_state = HostState {
        wasi: WasiCtxBuilder::new().inherit_stderr().build(),
        table: ResourceTable::new(),
        kv_prefix,
        redis_client: state.redis_client.clone(),
        nats_client,
        app_name,
        app_namespace,
        function_name,
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
