use std::sync::{Arc, RwLock};

use anyhow::Result;
use wasmtime::{
    Engine, Store, StoreLimits, StoreLimitsBuilder,
    component::{Component, Linker, ResourceTable},
};
use wasmtime_wasi::{WasiCtx, WasiCtxBuilder, WasiCtxView, WasiView};

use crate::config::FunctionEntry;
use crate::invocation::Invocation;
use crate::metrics::MetricsRegistry;
use crate::sql_pool::SqlPoolMap;

pub use platform_common::http_types::{HttpRequestPayload, HttpResponsePayload};

// Bindings for `world message-application` — binary payload in/out.
pub(crate) mod message_bindings {
    wasmtime::component::bindgen!({
        world: "message-application",
        path: "../../framework/runtime.wit",
        imports: {
            "framework:runtime/sql": async,
            "framework:runtime/kv": async,
            "framework:runtime/messaging": async,
        },
        exports: { default: async },
    });
}

// Bindings for `world http-application` — typed HTTP request/response.
mod http_bindings {
    wasmtime::component::bindgen!({
        world: "http-application",
        path: "../../framework/runtime.wit",
        imports: {
            "framework:runtime/sql": async,
            "framework:runtime/kv": async,
            "framework:runtime/messaging": async,
        },
        exports: { default: async },
    });
}

// ── Host state ────────────────────────────────────────────────────────────────
// One instance per request/call.

pub(crate) struct HostState {
    pub(crate) invocation: Invocation,
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
// threads.  Building the Linker once here avoids repeating add_to_linker_async
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
    _epoch_ticker: EpochTicker,
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
        wasmtime_wasi::p2::add_to_linker_async(&mut linker)?;
        message_bindings::framework::runtime::kv::add_to_linker::<
            HostState,
            wasmtime::component::HasSelf<HostState>,
        >(&mut linker, |h: &mut HostState| h)?;
        message_bindings::framework::runtime::messaging::add_to_linker::<
            HostState,
            wasmtime::component::HasSelf<HostState>,
        >(&mut linker, |h: &mut HostState| h)?;
        message_bindings::framework::runtime::log::add_to_linker::<
            HostState,
            wasmtime::component::HasSelf<HostState>,
        >(&mut linker, |h: &mut HostState| h)?;
        message_bindings::framework::runtime::metrics::add_to_linker::<
            HostState,
            wasmtime::component::HasSelf<HostState>,
        >(&mut linker, |h: &mut HostState| h)?;
        message_bindings::framework::runtime::sql::add_to_linker::<
            HostState,
            wasmtime::component::HasSelf<HostState>,
        >(&mut linker, |h: &mut HostState| h)?;
        let epoch_ticker = EpochTicker::new(engine.clone())?;
        Ok(Self {
            engine,
            linker,
            redis_client,
            sql_pools,
            metrics_registry,
            fuel_limit,
            memory_limit_bytes,
            _epoch_ticker: epoch_ticker,
        })
    }
}

struct EpochTicker {
    stop: std::sync::mpsc::Sender<()>,
    thread: Option<std::thread::JoinHandle<()>>,
}

impl EpochTicker {
    fn new(engine: Engine) -> Result<Self> {
        let (stop, rx) = std::sync::mpsc::channel();
        let thread = std::thread::Builder::new()
            .name("wasm-epoch".to_owned())
            .spawn(move || {
                while matches!(
                    rx.recv_timeout(std::time::Duration::from_millis(10)),
                    Err(std::sync::mpsc::RecvTimeoutError::Timeout)
                ) {
                    engine.increment_epoch();
                }
            })?;
        Ok(Self {
            stop,
            thread: Some(thread),
        })
    }
}

impl Drop for EpochTicker {
    fn drop(&mut self) {
        let _ = self.stop.send(());
        if let Some(thread) = self.thread.take() {
            let _ = thread.join();
        }
    }
}

// ── WASM invocations ──────────────────────────────────────────────────────────

pub(crate) struct InvocationContext {
    pub(crate) function: FunctionEntry,
    pub(crate) nats_client: Option<async_nats::Client>,
    pub(crate) invocation: Invocation,
}

impl RuntimeState {
    fn invocation_store(&self, context: InvocationContext) -> Result<Store<HostState>> {
        let InvocationContext {
            function,
            nats_client,
            invocation,
        } = context;
        invocation.check().map_err(anyhow::Error::msg)?;
        let sql_pool = function.sql_username.as_deref().and_then(|username| {
            self.sql_pools
                .get(&function.app_namespace, &function.app_name, username)
        });
        let redis_client = if function.kv_enabled {
            self.redis_client
                .read()
                .ok()
                .and_then(|client| client.clone())
        } else {
            None
        };
        let host_state = HostState {
            invocation,
            wasi: WasiCtxBuilder::new().build(),
            table: ResourceTable::new(),
            store_limits: StoreLimitsBuilder::new()
                .memory_size(self.memory_limit_bytes)
                .build(),
            redis_client,
            nats_client,
            sql_pool,
            app_name: function.app_name,
            app_namespace: function.app_namespace,
            function_name: function.function_name,
            metrics_registry: self.metrics_registry.clone(),
        };
        let mut store = Store::new(&self.engine, host_state);
        store.limiter(|host| &mut host.store_limits);
        store.set_epoch_deadline(1);
        store.epoch_deadline_async_yield_and_update(1);
        if let Some(fuel) = self.fuel_limit {
            store.set_fuel(fuel)?;
        }
        Ok(store)
    }
}

pub async fn invoke_on_message(
    state: &RuntimeState,
    component: &Component,
    payload: &[u8],
    context: InvocationContext,
) -> Result<Option<Vec<u8>>> {
    let invocation = context.invocation.clone();
    let mut store = state.invocation_store(context)?;

    let result = invocation
        .run(async {
            let app = message_bindings::MessageApplication::instantiate_async(
                &mut store,
                component,
                &state.linker,
            )
            .await?;
            app.call_on_message(&mut store, payload).await
        })
        .await
        .map_err(anyhow::Error::msg)??;

    result.map_err(|msg| anyhow::anyhow!("component returned error: {msg}"))
}

pub async fn invoke_on_request(
    state: &RuntimeState,
    component: &Component,
    request: HttpRequestPayload,
    context: InvocationContext,
) -> Result<HttpResponsePayload> {
    let invocation = context.invocation.clone();
    let app_name = context.function.app_name.clone();
    let app_namespace = context.function.app_namespace.clone();
    let mut store = state.invocation_store(context)?;

    let wit_request = http_bindings::HttpRequest {
        method: request.method,
        path: request.path,
        query: request.query,
        headers: request.headers,
        body: request.body,
    };

    let result = invocation
        .run(async {
            let app = http_bindings::HttpApplication::instantiate_async(
                &mut store,
                component,
                &state.linker,
            )
            .await?;
            app.call_on_request(&mut store, &wit_request).await
        })
        .await
        .map_err(anyhow::Error::msg)??;

    match result {
        Ok(wit_response) => {
            state.metrics_registry.record_http_request(
                &app_name,
                &app_namespace,
                wit_response.status,
            );
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
            matches!(
                err.downcast_ref::<wasmtime::Trap>(),
                Some(wasmtime::Trap::OutOfFuel)
            ),
            "expected out-of-fuel trap, got: {err:#}",
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
