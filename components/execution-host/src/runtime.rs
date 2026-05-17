use std::sync::{Arc, RwLock};

use anyhow::Result;
use wasmtime::
    {Engine, Store, StoreLimits, StoreLimitsBuilder,
    component::{Component, Linker, ResourceTable},
};
use wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView};

use crate::metrics::MetricsRegistry;
use crate::sql_pool::SqlPoolMap;

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
    store_limits: StoreLimits,
    pub(crate) redis_client: Option<redis::Client>,
    pub(crate) nats_client: Option<async_nats::Client>,
    pub(crate) sql_pool: Option<sqlx::postgres::PgPool>,
    pub(crate) app_name: String,
    pub(crate) app_namespace: String,
    pub(crate) function_name: String,
    pub(crate) metrics_registry: MetricsRegistry,
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
//
// `redis_client` is behind an Arc<RwLock<…>> so that it can be updated
// dynamically when the configsync stream delivers new Redis credentials,
// without requiring a restart.

pub struct RuntimeState {
    pub engine: Engine,
    linker: Linker<HostState>,
    /// Current Redis client, swapped by the configsync→Redis watcher task.
    pub redis_client: Arc<RwLock<Option<redis::Client>>>,
    pub sql_pools: Arc<SqlPoolMap>,
    pub metrics_registry: MetricsRegistry,
    fuel_limit: Option<u64>,
    memory_limit_bytes: usize,
}

impl RuntimeState {
    pub fn new(
        engine: Engine,
        redis_client: Arc<RwLock<Option<redis::Client>>>,
        metrics_registry: MetricsRegistry,
        sql_pools: Arc<SqlPoolMap>,
        fuel_limit: Option<u64>,
        memory_limit_bytes: usize,
    ) -> Result<Self> {
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
        message_bindings::framework::runtime::metrics::add_to_linker::<HostState, wasmtime::component::HasSelf<HostState>>(
            &mut linker,
            |h: &mut HostState| h,
        )?;
        message_bindings::framework::runtime::sql::add_to_linker::<HostState, wasmtime::component::HasSelf<HostState>>(
            &mut linker,
            |h: &mut HostState| h,
        )?;
        Ok(Self { engine, linker, redis_client, sql_pools, metrics_registry, fuel_limit, memory_limit_bytes })
    }
}

// ── WASM invocations ──────────────────────────────────────────────────────────

pub fn invoke_on_message(
    state: &RuntimeState,
    component: &Component,
    payload: &[u8],
    nats_client: Option<async_nats::Client>,
    app_name: String,
    app_namespace: String,
    function_name: String,
    sql_username: Option<String>,
) -> Result<Option<Vec<u8>>> {
    let sql_pool = sql_username
        .as_deref()
        .and_then(|u| state.sql_pools.get(&app_namespace, &app_name, u));
    let redis_client = state.redis_client.read().ok().and_then(|g| g.clone());
    let host_state = HostState {
        wasi: WasiCtxBuilder::new().inherit_stderr().build(),
        table: ResourceTable::new(),
        store_limits: StoreLimitsBuilder::new()
            .memory_size(state.memory_limit_bytes)
            .build(),
        redis_client,
        nats_client,
        sql_pool,
        app_name,
        app_namespace,
        function_name,
        metrics_registry: state.metrics_registry.clone(),
    };
    let mut store = Store::new(&state.engine, host_state);
    store.limiter(|h| &mut h.store_limits);
    if let Some(fuel) = state.fuel_limit {
        store.set_fuel(fuel)?;
    }

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
    nats_client: Option<async_nats::Client>,
    app_name: String,
    app_namespace: String,
    function_name: String,
    sql_username: Option<String>,
) -> Result<HttpResponsePayload> {
    let sql_pool = sql_username
        .as_deref()
        .and_then(|u| state.sql_pools.get(&app_namespace, &app_name, u));
    let redis_client = state.redis_client.read().ok().and_then(|g| g.clone());
    let host_state = HostState {
        wasi: WasiCtxBuilder::new().inherit_stderr().build(),
        table: ResourceTable::new(),
        store_limits: StoreLimitsBuilder::new()
            .memory_size(state.memory_limit_bytes)
            .build(),
        redis_client,
        nats_client,
        sql_pool,
        app_name: app_name.clone(),
        app_namespace: app_namespace.clone(),
        function_name,
        metrics_registry: state.metrics_registry.clone(),
    };
    let mut store = Store::new(&state.engine, host_state);
    store.limiter(|h| &mut h.store_limits);
    if let Some(fuel) = state.fuel_limit {
        store.set_fuel(fuel)?;
    }

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
        Ok(wit_response) => {
            state.metrics_registry.record_http_request(&app_name, &app_namespace, wit_response.status);
            Ok(HttpResponsePayload {
                status: wit_response.status,
                headers: wit_response.headers,
                body: wit_response.body,
            })
        }
        Err(msg) => Err(anyhow::anyhow!("component returned error: {msg}")),
    }
}

#[cfg(test)]
mod tests {
    use wasmtime::{Engine, Instance, Module, Store, StoreLimitsBuilder};

    /// A plain (non-component) engine with fuel metering enabled.
    fn fuel_engine() -> Engine {
        let mut config = wasmtime::Config::new();
        config.consume_fuel(true);
        Engine::new(&config).unwrap()
    }

    #[test]
    fn infinite_loop_is_killed_by_fuel() {
        let engine = fuel_engine();
        // Start function loops unconditionally; fuel exhaustion traps.
        let module = Module::new(
            &engine,
            r#"(module (func $loop (loop (br 0))) (start $loop))"#,
        )
        .unwrap();
        let mut store = Store::new(&engine, ());
        store.set_fuel(10_000).unwrap();
        let err = Instance::new(&mut store, &module, &[]).unwrap_err();
        assert!(
            err.to_string().contains("fuel"),
            "expected fuel trap, got: {err}",
        );
    }

    #[test]
    fn memory_beyond_limit_is_rejected() {
        struct LimitedState {
            limits: wasmtime::StoreLimits,
        }
        let engine = Engine::default();
        // 2 000 pages × 64 KiB = 128 MiB; the 64 MiB limit rejects this.
        let module = Module::new(&engine, "(module (memory 2000))").unwrap();
        let limits = StoreLimitsBuilder::new()
            .memory_size(64 * 1024 * 1024)
            .build();
        let mut store = Store::new(&engine, LimitedState { limits });
        store.limiter(|s| &mut s.limits);
        let err = Instance::new(&mut store, &module, &[]).unwrap_err();
        assert!(
            err.to_string().to_lowercase().contains("memory"),
            "expected memory limit error, got: {err}",
        );
    }
}
