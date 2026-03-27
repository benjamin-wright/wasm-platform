use anyhow::Result;
use axum::{Router, routing::get};
use futures_util::StreamExt as _;
use std::sync::Arc;
use tokio::sync::Semaphore;
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

    let state = Arc::new(RuntimeState { engine, component });

    tracing::info!("execution-host starting");

    // NATS connection — credentials are injected from the db-operator-managed
    // secret.  The URL is constructed here so it is never exposed as a
    // pre-composed environment variable containing the password.
    let nats_url = build_nats_url()?;
    let nats_topic = std::env::var("NATS_TOPIC").unwrap_or_else(|_| "execute".to_string());

    let nats_client = async_nats::connect(&nats_url).await?;
    tracing::info!(%nats_topic, "connected to NATS");

    let mut subscriber = nats_client.subscribe(nats_topic).await?;

    // Bound concurrent WASM invocations to avoid resource exhaustion under
    // high message volume.  Each permit corresponds to one in-flight guest
    // execution; callers that cannot acquire a permit are dropped with a log.
    let max_concurrent = std::env::var("MAX_CONCURRENT_INVOCATIONS")
        .ok()
        .and_then(|v| v.parse::<usize>().ok())
        .unwrap_or(64);
    let semaphore = Arc::new(Semaphore::new(max_concurrent));

    // Health-check HTTP server — Kubernetes liveness/readiness probes hit
    // /healthz on port 3000.  This runs concurrently with the NATS loop.
    let health_app = Router::new().route("/healthz", get(healthz_handler));
    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000").await?;
    let health_server = axum::serve(listener, health_app);

    tokio::select! {
        result = health_server => {
            result?;
        }
        _ = async {
            while let Some(message) = subscriber.next().await {
                let permit = match semaphore.clone().try_acquire_owned() {
                    Ok(p) => p,
                    Err(_) => {
                        tracing::warn!("max concurrent invocations reached; dropping message");
                        continue;
                    }
                };

                let state = Arc::clone(&state);
                let reply = message.reply.clone();
                let client = nats_client.clone();
                let payload = message.payload.to_vec();

                // WASM execution is CPU-bound; run it on the blocking thread
                // pool so the async runtime stays responsive.
                tokio::spawn(async move {
                    let _permit = permit; // held until the spawn completes
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
                });
            }
        } => {}
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
