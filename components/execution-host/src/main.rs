mod config_sync;

use anyhow::Result;
use axum::{Router, routing::get};
use futures_util::StreamExt as _;
use std::sync::{Arc, RwLock};
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
// The Engine is expensive to create and is safe to share across threads.
// The Component is the pre-compiled wasm binary, also shareable.

struct RuntimeState {
    engine: Engine,
    component: Component,
    #[allow(dead_code)]
    config: Arc<RwLock<config_sync::AppConfig>>,
}

// ── Entry point ───────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let mut config = wasmtime::Config::new();
    config.wasm_component_model(true);
    let engine = Engine::new(&config)?;

    // Module path is configured via WASM_MODULE_PATH. Defaults to the local
    // build output so `make run` works without any extra configuration.
    let wasm_path = std::env::var("WASM_MODULE_PATH").unwrap_or_else(|_| {
        concat!(
            env!("CARGO_MANIFEST_DIR"),
            "/../../target/wasm32-wasip2/release/hello_world.wasm"
        )
        .to_string()
    });
    // .cwasm files are AOT-compiled artifacts produced by the precompile
    // binary; .wasm files are compiled at load time (used in local dev).
    let component = if wasm_path.ends_with(".cwasm") {
        // Safety: the file was produced by the precompile binary using the
        // same Engine configuration and Wasmtime version as this process.
        unsafe { Component::deserialize_file(&engine, &wasm_path)? }
    } else {
        Component::from_file(&engine, &wasm_path)?
    };

    // Config sync — the shared state is populated on startup by the full-config
    // RPC and kept up to date via the incremental update stream.
    let app_config = Arc::new(RwLock::new(config_sync::AppConfig::default()));
    let operator_endpoint = std::env::var("OPERATOR_GRPC_ENDPOINT")
        .unwrap_or_else(|_| "http://wp-operator:50051".to_string());
    // POD_NAME is injected by the Kubernetes downward API (fieldRef: metadata.name)
    // and is used as the host identifier in gRPC messages to the operator.
    let host_id = std::env::var("POD_NAME").unwrap_or_else(|_| "execution-host-local".to_string());

    tokio::spawn(config_sync::sync(
        operator_endpoint,
        host_id,
        Arc::clone(&app_config),
    ));

    let state = Arc::new(RuntimeState {
        engine,
        component,
        config: Arc::clone(&app_config),
    });

    tracing::info!("execution-host starting");

    // NATS connection — credentials are injected from the db-operator-managed
    // secret.  The URL is constructed here so it is never exposed as a
    // pre-composed environment variable containing the password.
    let nats_url = build_nats_url()?;
    // Subscribe to all subjects under the configured prefix using the NATS `>`
    // wildcard.  Each application publishes to `{prefix}{spec.topic}`, so a
    // single wildcard subscription covers all deployed apps while reserving
    // other subject namespaces for other platform components.
    let topic_prefix = std::env::var("NATS_TOPIC_PREFIX").unwrap_or_else(|_| "fn.".to_string());
    let nats_subject = format!("{}>", topic_prefix);

    let nats_client = async_nats::connect(&nats_url).await?;
    tracing::info!(%nats_subject, "connected to NATS");

    let subscriber = nats_client.subscribe(nats_subject).await?;

    let max_concurrent = std::env::var("MAX_CONCURRENT_INVOCATIONS")
        .ok()
        .and_then(|v| v.parse::<usize>().ok())
        .unwrap_or(64);

    // Health-check HTTP server — Kubernetes liveness/readiness probes hit
    // /healthz on port 3000.  This runs concurrently with the NATS loop.
    let health_app = Router::new().route("/healthz", get(healthz_handler));
    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000").await?;
    let health_server = axum::serve(listener, health_app);

    tokio::select! {
        result = health_server => {
            result?;
        }
        // for_each_concurrent drives the subscriber stream with a built-in
        // concurrency limit; it back-pressures the NATS client when all slots
        // are occupied, preventing unbounded resource consumption.
        _ = subscriber.for_each_concurrent(max_concurrent, |message| {
            let state = Arc::clone(&state);
            let client = nats_client.clone();
            async move {
                let reply = message.reply.clone();
                let payload = message.payload.to_vec();

                // WASM execution is CPU-bound; run it on the blocking thread
                // pool so the async runtime stays responsive.
                let result = tokio::task::spawn_blocking(move || {
                    invoke_on_message(&state, &payload)
                })
                .await;

                match result {
                    Ok(Ok(Some(response_body))) => {
                        if let Some(reply_subject) = reply
                            && let Err(err) = client
                                .publish(reply_subject.clone(), response_body.into())
                                .await
                        {
                            tracing::error!(
                                %reply_subject,
                                "failed to publish reply: {err:#}"
                            );
                        }
                    }
                    Ok(Ok(None)) => {}
                    Ok(Err(err)) => {
                        tracing::error!("invoke_on_message failed: {err:#}");
                    }
                    Err(join_err) => {
                        tracing::error!("spawn_blocking panicked: {join_err}");
                    }
                }
            }
        }) => {}
    }

    Ok(())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// Constructs the NATS URL from the individual credential environment variables
// populated by the db-operator secret, falling back to a local development
// default when none are set.  Building the URL in code (rather than via
// shell variable interpolation in the Helm chart) keeps the composed URL —
// which embeds the password — out of the process environment.
fn build_nats_url() -> Result<String> {
    if let (Ok(username), Ok(password), Ok(host), Ok(port)) = (
        std::env::var("NATS_USERNAME"),
        std::env::var("NATS_PASSWORD"),
        std::env::var("NATS_HOST"),
        std::env::var("NATS_PORT"),
    ) {
        return Ok(format!(
            "nats://{}:{}@{}:{}",
            username, password, host, port
        ));
    }
    Ok("nats://localhost:4222".to_string())
}

// ── Handlers ──────────────────────────────────────────────────────────────────

async fn healthz_handler() -> &'static str {
    tracing::info!("healthz_handler called");
    "OK"
}

// ── WASM invocation ───────────────────────────────────────────────────────────

fn invoke_on_message(state: &RuntimeState, payload: &[u8]) -> Result<Option<Vec<u8>>> {
    let mut linker: Linker<HostState> = Linker::new(&state.engine);

    // Add WASI host functions.  When kv/sql/messaging are ready, call their
    // equivalent `add_to_linker` generated by bindgen! (or a hand-written one)
    // here.
    wasmtime_wasi::p2::add_to_linker_sync(&mut linker)?;

    let host_state = HostState {
        wasi: WasiCtxBuilder::new().inherit_stderr().build(),
        table: ResourceTable::new(),
    };
    let mut store = Store::new(&state.engine, host_state);

    let app = Application::instantiate(&mut store, &state.component, &linker)?;

    let result = app.call_on_message(&mut store, payload)?;

    result.map_err(|msg| anyhow::anyhow!("component returned error: {msg}"))
}
