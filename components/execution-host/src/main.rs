mod config;
mod config_sync;
mod module_cache;
mod modules;
mod nats;
mod oci;
mod runtime;

use anyhow::Result;
use axum::{Router, routing::get};
use config::AppRegistry;
use modules::ModuleRegistry;
use runtime::{RuntimeState, invoke_on_message};
use std::sync::Arc;
use wasmtime::Engine;

// ── Entry point ───────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let mut wasm_config = wasmtime::Config::new();
    wasm_config.wasm_component_model(true);
    let engine = Engine::new(&wasm_config)?;

    let state = Arc::new(RuntimeState::new(engine.clone())?);

    // MODULE_CACHE_ADDR is required — without a module cache the host cannot
    // load compiled WASM artifacts for any application.
    let cache_addr = std::env::var("MODULE_CACHE_ADDR")
        .map_err(|_| anyhow::anyhow!("MODULE_CACHE_ADDR environment variable is required"))?;

    let module_registry = ModuleRegistry::new(cache_addr, engine);

    tracing::info!("execution-host starting");

    // Build the app registry and populate it with the operator's current config
    // snapshot.  CONFIG_SYNC_ADDR is required — the host cannot serve any apps
    // without knowing which topics to subscribe to.
    let addr = std::env::var("CONFIG_SYNC_ADDR")
        .map_err(|_| anyhow::anyhow!("CONFIG_SYNC_ADDR environment variable is required"))?;
    let host_id = std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown".to_string());
    let app_registry = AppRegistry::new();
    // Watch channel for topic-set changes published by the config sync loop.
    let (topics_tx, topics_rx) = tokio::sync::watch::channel(Vec::<String>::new());
    // Channel through which all per-topic NATS subscribers forward messages.
    let (msg_tx, msg_rx) = tokio::sync::mpsc::channel::<async_nats::Message>(256);

    // NATS connection — credentials are injected from the db-operator-managed secret.
    let nats_client = nats::connect().await?;
    tracing::info!("connected to NATS");

    // Config sync loop — fetches a full snapshot on startup then maintains the
    // incremental update stream, reconnecting automatically on failure.
    tokio::spawn(config_sync::run_config_sync_loop(
        addr,
        host_id,
        app_registry.clone(),
        module_registry.clone(),
        topics_tx,
    ));
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
        _ = process_nats_messages(msg_rx, Arc::clone(&state), app_registry, module_registry, nats_client, max_concurrent) => {}
    }

    Ok(())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// Receives NATS messages from all per-topic subscribers and dispatches them
// concurrently up to max_concurrent in-flight invocations.
async fn process_nats_messages(
    mut msg_rx: tokio::sync::mpsc::Receiver<async_nats::Message>,
    state: Arc<RuntimeState>,
    app_registry: AppRegistry,
    module_registry: ModuleRegistry,
    client: async_nats::Client,
    max_concurrent: usize,
) {
    let semaphore = Arc::new(tokio::sync::Semaphore::new(max_concurrent));
    while let Some(message) = msg_rx.recv().await {
        let permit = match Arc::clone(&semaphore).acquire_owned().await {
            Ok(p) => p,
            Err(_) => break,
        };

        // Look up which app is subscribed to this topic and find its compiled module.
        let subject = message.subject.to_string();
        let app_config = match app_registry.get_by_topic(&subject) {
            Ok(Some(cfg)) => cfg,
            Ok(None) => {
                tracing::warn!(%subject, "received message for unknown topic; dropping");
                continue;
            }
            Err(err) => {
                tracing::error!(%subject, "app registry error: {err:#}");
                continue;
            }
        };
        let component = match module_registry.get(&app_config.namespace, &app_config.name) {
            Ok(Some(c)) => c,
            Ok(None) => {
                tracing::warn!(
                    namespace = %app_config.namespace,
                    name = %app_config.name,
                    "module not yet loaded; dropping message"
                );
                continue;
            }
            Err(err) => {
                tracing::error!("module registry error: {err:#}");
                continue;
            }
        };

        let state = Arc::clone(&state);
        let client = client.clone();
        tokio::spawn(async move {
            let _permit = permit;
            let reply = message.reply.clone();
            let payload = message.payload.to_vec();

            // WASM execution is CPU-bound; run it on the blocking thread
            // pool so the async runtime stays responsive.
            let result =
                tokio::task::spawn_blocking(move || invoke_on_message(&state, &component, &payload))
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

