mod config;

use anyhow::Result;
use axum::{Router, routing::get};
use config::{
    AppRegistry,
    configsync::{FullConfigRequest, IncrementalUpdateAck, config_sync_client::ConfigSyncClient},
};
use futures_util::StreamExt as _;
use std::{collections::{HashMap, HashSet}, sync::Arc, time::Duration};
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

    // Build the app registry and populate it with the operator's current config
    // snapshot.  CONFIG_SYNC_ADDR is required — the host cannot serve any apps
    // without knowing which topics to subscribe to.
    let addr = std::env::var("CONFIG_SYNC_ADDR")
        .map_err(|_| anyhow::anyhow!("CONFIG_SYNC_ADDR environment variable is required"))?;
    let host_id = std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown".to_string());
    let registry = AppRegistry::new();
    // Watch channel for topic-set changes published by the config sync loop.
    let (topics_tx, topics_rx) = tokio::sync::watch::channel(Vec::<String>::new());
    // Channel through which all per-topic NATS subscribers forward messages.
    let (msg_tx, msg_rx) = tokio::sync::mpsc::channel::<async_nats::Message>(256);

    // NATS connection — credentials are injected from the db-operator-managed
    // secret.  Credentials are passed via ConnectOptions rather than embedded
    // in the URL; async-nats 0.46 does not parse user:pass from URL strings.
    let (nats_url, nats_opts) = build_nats_connect_config()?;
    let nats_client = nats_opts.connect(&nats_url).await?;
    tracing::info!("connected to NATS");

    // Config sync loop — fetches a full snapshot on startup then maintains the
    // incremental update stream, reconnecting automatically on failure.
    tokio::spawn(run_config_sync_loop(addr, host_id, registry, topics_tx));
    // Subscription manager — watches the topic set and subscribes/unsubscribes
    // NATS subjects, forwarding all messages into msg_tx.
    tokio::spawn(manage_nats_subscriptions(nats_client.clone(), topics_rx, msg_tx));

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
        _ = process_nats_messages(msg_rx, Arc::clone(&state), nats_client, max_concurrent) => {}
    }

    Ok(())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// Loops forever: fetches a full config snapshot then maintains the incremental
// update stream.  On any error or clean stream close, backs off and retries.
async fn run_config_sync_loop(
    addr: String,
    host_id: String,
    registry: AppRegistry,
    topics_tx: tokio::sync::watch::Sender<Vec<String>>,
) {
    let mut backoff = Duration::from_secs(1);
    loop {
        match run_config_sync(&addr, &host_id, &registry, &topics_tx).await {
            Ok(()) => {
                tracing::warn!("config sync stream closed; reconnecting");
                backoff = Duration::from_secs(1);
            }
            Err(err) => {
                tracing::warn!("config sync error: {err:#}; reconnecting in {backoff:?}");
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        }
        tokio::time::sleep(backoff).await;
    }
}

// Fetches a full config snapshot then drives a single incremental update stream
// session until it closes or errors.
async fn run_config_sync(
    addr: &str,
    host_id: &str,
    registry: &AppRegistry,
    topics_tx: &tokio::sync::watch::Sender<Vec<String>>,
) -> Result<()> {
    fetch_full_config(addr.to_string(), host_id.to_string(), registry).await?;
    topics_tx.send(registry.topics()).ok();

    tracing::info!("opening incremental update stream");
    let mut client = ConfigSyncClient::connect(addr.to_string()).await?;

    // Bridge tokio mpsc → futures Stream so tonic can consume acks.
    let (ack_tx, ack_rx) = tokio::sync::mpsc::channel::<IncrementalUpdateAck>(16);
    let ack_stream = futures_util::stream::unfold(ack_rx, |mut rx| async move {
        rx.recv().await.map(|ack| (ack, rx))
    });

    let mut update_stream = client
        .push_incremental_update(ack_stream)
        .await?
        .into_inner();

    while let Some(request) = update_stream.message().await? {
        if let Some(incremental) = request.incremental_config {
            let version = incremental.version.clone();
            let update_count = incremental.updates.len();
            registry.apply_incremental(incremental.updates);
            topics_tx.send(registry.topics()).ok();
            tracing::debug!(version, update_count, "incremental config applied");
            let ack = IncrementalUpdateAck {
                host_id: host_id.to_string(),
                version_applied: version,
                success: true,
                message: String::new(),
            };
            if ack_tx.send(ack).await.is_err() {
                break;
            }
        }
    }
    Ok(())
}

// Connects to the operator's gRPC endpoint, requests a full config snapshot,
// and applies it to the registry.  Returns an error if the connection or RPC
// fails so that the process exits rather than silently running unconfigured.
async fn fetch_full_config(addr: String, host_id: String, registry: &AppRegistry) -> Result<()> {
    tracing::info!(%addr, "connecting to operator for full config");
    let mut client = ConfigSyncClient::connect(addr).await?;
    let response = client
        .request_full_config(FullConfigRequest {
            host_id,
            last_ack_timestamp: None,
        })
        .await?
        .into_inner();
    if let Some(full) = response.config {
        let app_count = full.applications.len();
        registry.apply_full_config(full);
        tracing::info!(app_count, "full config applied");
    } else {
        tracing::warn!("operator returned empty full config response");
    }
    Ok(())
}

// Returns the NATS server URL and a ConnectOptions configured with credentials
// from the db-operator-managed secret, falling back to unauthenticated
// localhost for local development.  Credentials are passed through
// ConnectOptions rather than embedded in the URL; async-nats 0.46 does not
// parse user:pass from a URL string.
fn build_nats_connect_config() -> Result<(String, async_nats::ConnectOptions)> {
    if let (Ok(username), Ok(password), Ok(host), Ok(port)) = (
        std::env::var("NATS_USERNAME"),
        std::env::var("NATS_PASSWORD"),
        std::env::var("NATS_HOST"),
        std::env::var("NATS_PORT"),
    ) {
        let url = format!("nats://{}:{}", host, port);
        let opts = async_nats::ConnectOptions::new().user_and_password(username, password);
        return Ok((url, opts));
    }
    Ok(("nats://localhost:4222".to_string(), async_nats::ConnectOptions::new()))
}

// Watches the topic set published by the config sync loop and maintains one
// NATS subscription per topic.  All messages are forwarded into msg_tx.
async fn manage_nats_subscriptions(
    client: async_nats::Client,
    mut topics_rx: tokio::sync::watch::Receiver<Vec<String>>,
    msg_tx: tokio::sync::mpsc::Sender<async_nats::Message>,
) {
    // Keys are topic strings; values are oneshot senders used to cancel the
    // per-topic forwarding task (dropping the sender signals the task to stop,
    // which drops the Subscriber and sends UNSUB to the server).
    let mut subscriptions: HashMap<String, tokio::sync::oneshot::Sender<()>> = HashMap::new();

    loop {
        if topics_rx.changed().await.is_err() {
            break;
        }
        let desired: HashSet<String> = topics_rx.borrow_and_update().iter().cloned().collect();
        let current: HashSet<String> = subscriptions.keys().cloned().collect();

        for topic in current.difference(&desired) {
            subscriptions.remove(topic);
            tracing::info!(%topic, "unsubscribed from NATS topic");
        }

        for topic in desired.difference(&current) {
            match client.subscribe(topic.clone()).await {
                Ok(sub) => {
                    let (cancel_tx, cancel_rx) = tokio::sync::oneshot::channel::<()>();
                    let tx = msg_tx.clone();
                    let t = topic.clone();
                    tokio::spawn(async move {
                        let mut sub = sub;
                        tokio::select! {
                            _ = cancel_rx => {}
                            _ = async move {
                                while let Some(msg) = sub.next().await {
                                    if tx.send(msg).await.is_err() {
                                        break;
                                    }
                                }
                            } => {}
                        }
                    });
                    subscriptions.insert(topic.clone(), cancel_tx);
                    tracing::info!(%t, "subscribed to NATS topic");
                }
                Err(err) => {
                    tracing::error!(%topic, "failed to subscribe to NATS topic: {err:#}");
                }
            }
        }
    }
}

// Receives NATS messages from all per-topic subscribers and dispatches them
// concurrently up to max_concurrent in-flight invocations.
async fn process_nats_messages(
    mut msg_rx: tokio::sync::mpsc::Receiver<async_nats::Message>,
    state: Arc<RuntimeState>,
    client: async_nats::Client,
    max_concurrent: usize,
) {
    let semaphore = Arc::new(tokio::sync::Semaphore::new(max_concurrent));
    while let Some(message) = msg_rx.recv().await {
        let permit = match Arc::clone(&semaphore).acquire_owned().await {
            Ok(p) => p,
            Err(_) => break,
        };
        let state = Arc::clone(&state);
        let client = client.clone();
        tokio::spawn(async move {
            let _permit = permit;
            let reply = message.reply.clone();
            let payload = message.payload.to_vec();

            // WASM execution is CPU-bound; run it on the blocking thread
            // pool so the async runtime stays responsive.
            let result = tokio::task::spawn_blocking(move || invoke_on_message(&state, &payload))
                .await;

            match result {
                Ok(Ok(Some(response_body))) => {
                    if let Some(reply_subject) = reply
                        && let Err(err) = client
                            .publish(reply_subject.clone(), response_body.into())
                            .await
                    {
                        tracing::error!(%reply_subject, "failed to publish reply: {err:#}");
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
