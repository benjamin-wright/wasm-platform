mod config;
mod config_sync;
mod nats;
mod runtime;

use anyhow::Result;
use axum::{Router, routing::get};
use config::AppRegistry;
use runtime::{RuntimeState, invoke_on_message};
use std::sync::Arc;
use wasmtime::{Engine, component::Component};

// ── Entry point ───────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let mut wasm_config = wasmtime::Config::new();
    wasm_config.wasm_component_model(true);
    let engine = Engine::new(&wasm_config)?;

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

    let state = Arc::new(RuntimeState::new(engine, component)?);

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

    // NATS connection — credentials are injected from the db-operator-managed secret.
    let nats_client = nats::connect().await?;
    tracing::info!("connected to NATS");

    // Config sync loop — fetches a full snapshot on startup then maintains the
    // incremental update stream, reconnecting automatically on failure.
    tokio::spawn(config_sync::run_config_sync_loop(addr, host_id, registry, topics_tx));
    // Subscription manager — watches the topic set and subscribes/unsubscribes
    // NATS subjects, forwarding all messages into msg_tx.
    tokio::spawn(nats::manage_nats_subscriptions(nats_client.clone(), topics_rx, msg_tx));

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
    tracing::trace!("healthz_handler called");
    "OK"
}

