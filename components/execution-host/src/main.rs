mod config;
mod config_sync;
mod module_cache;
mod modules;
mod nats;
mod oci;
mod runtime;

use anyhow::Result;
use axum::{Router, extract::State, http::StatusCode, routing::get};
use config::AppRegistry;
use modules::ModuleRegistry;
use runtime::{RuntimeState, HttpRequestPayload, invoke_on_message, invoke_on_request};
use std::{path::PathBuf, sync::Arc};
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

    // NATS_CREDENTIALS_PATH must point to a directory containing NATS_USERNAME,
    // NATS_PASSWORD, NATS_HOST, and NATS_PORT files (Kubernetes secret volume
    // mount layout).  The manager re-reads this directory on every connection
    // attempt so that rotated credentials are picked up without pod restarts.
    let credentials_path = std::env::var("NATS_CREDENTIALS_PATH")
        .map(PathBuf::from)
        .map_err(|_| anyhow::anyhow!("NATS_CREDENTIALS_PATH environment variable is required"))?;

    let app_registry = AppRegistry::new();
    // Watch channel for topic-set changes published by the config sync loop.
    let (topics_tx, topics_rx) = tokio::sync::watch::channel(Vec::<String>::new());
    // Channel through which all per-topic NATS subscribers forward messages.
    let (msg_tx, msg_rx) = tokio::sync::mpsc::channel::<async_nats::Message>(256);

    // Readiness state — the pod is ready only when both NATS and the config
    // sync stream have been successfully established.
    let (nats_ready_tx, nats_ready_rx) = tokio::sync::watch::channel(false);
    let (synced_tx, synced_rx) = tokio::sync::watch::channel(false);

    // Live NATS client watch — None while the manager is (re)connecting.
    let (client_tx, client_rx) = tokio::sync::watch::channel::<Option<async_nats::Client>>(None);

    // NATS manager — connects, broadcasts the live client, and automatically
    // reconnects (re-reading credentials from disk) on auth failures.
    tokio::spawn(nats::run_nats_manager(
        credentials_path,
        client_tx,
        nats_ready_tx,
    ));

    // Config sync loop — fetches a full snapshot on startup then maintains the
    // incremental update stream, reconnecting automatically on failure.
    tokio::spawn(config_sync::run_config_sync_loop(
        addr,
        host_id,
        app_registry.clone(),
        module_registry.clone(),
        topics_tx,
        synced_tx,
    ));
    // Subscription manager — watches the client and topic set, maintaining one
    // NATS subscription per live topic; re-subscribes on client replacement.
    tokio::spawn(nats::manage_nats_subscriptions(client_rx.clone(), topics_rx, msg_tx));

    let max_concurrent = std::env::var("MAX_CONCURRENT_INVOCATIONS")
        .ok()
        .and_then(|v| v.parse::<usize>().ok())
        .unwrap_or(64);

    // Health-check HTTP server on port 3000.
    // /healthz — liveness probe: always 200 while the process is alive.
    // /readyz  — readiness probe: 200 only when NATS is connected AND the
    //            config sync loop has received at least one full snapshot.
    tokio::spawn(watch_readiness(
        nats_ready_rx.clone(),
        synced_rx.clone(),
        "config sync",
    ));
    let ready_state = ReadyState { nats_ready_rx, synced_rx };
    let health_app = Router::new()
        .route("/healthz", get(healthz_handler))
        .route("/readyz", get(readyz_handler))
        .with_state(ready_state);
    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000").await?;
    let health_server = axum::serve(listener, health_app);

    tokio::select! {
        result = health_server => {
            result?;
        }
        _ = process_nats_messages(msg_rx, Arc::clone(&state), app_registry, module_registry, client_rx, max_concurrent) => {}
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
    client_rx: tokio::sync::watch::Receiver<Option<async_nats::Client>>,
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
        // Snapshot the current client.  If NATS is mid-reconnect the snapshot
        // is None; replies will be silently dropped and the caller will time out.
        let client_snapshot = client_rx.borrow().clone();
        tokio::spawn(async move {
            let _permit = permit;
            let reply = message.reply.clone();
            let payload = message.payload.to_vec();

            // Dispatch based on the world type declared in the app's config.
            // In prost 0.13, enum fields are stored as i32; use TryFrom to
            // convert to the typed enum, defaulting to MESSAGE on unknown values.
            let world_type =
                config::configsync::WorldType::try_from(app_config.world_type)
                    .unwrap_or(config::configsync::WorldType::Message);

            // WASM execution is CPU-bound; run it on the blocking thread
            // pool so the async runtime stays responsive.
            let result = match world_type {
                config::configsync::WorldType::Message => {
                    tokio::task::spawn_blocking(move || {
                        invoke_on_message(&state, &component, &payload)
                    })
                    .await
                }
                config::configsync::WorldType::Http => {
                    tokio::task::spawn_blocking(move || {
                        let request: HttpRequestPayload =
                            serde_json::from_slice(&payload).map_err(|e| {
                                anyhow::anyhow!("failed to decode HTTP request payload: {e}")
                            })?;
                        let response = invoke_on_request(&state, &component, request)?;
                        let bytes = serde_json::to_vec(&response).map_err(|e| {
                            anyhow::anyhow!("failed to encode HTTP response payload: {e}")
                        })?;
                        Ok(Some(bytes))
                    })
                    .await
                }
            };

            match result {
                Ok(Ok(Some(response_body))) => {
                    if let Some(reply_subject) = reply {
                        if let Some(client) = client_snapshot {
                            if let Err(err) = client
                                .publish(reply_subject.clone(), response_body.into())
                                .await
                            {
                                tracing::error!(%reply_subject, "failed to publish reply: {err:#}");
                            }
                        } else {
                            tracing::warn!(%reply_subject, "NATS unavailable; dropping reply (caller will time out)");
                        }
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

#[derive(Clone)]
struct ReadyState {
    nats_ready_rx: tokio::sync::watch::Receiver<bool>,
    synced_rx: tokio::sync::watch::Receiver<bool>,
}

async fn healthz_handler() -> &'static str {
    tracing::trace!("healthz_handler called");
    "OK"
}

async fn readyz_handler(
    State(state): State<ReadyState>,
) -> (StatusCode, &'static str) {
    let nats_ready = *state.nats_ready_rx.borrow();
    let synced = *state.synced_rx.borrow();
    if nats_ready && synced {
        (StatusCode::OK, "ready")
    } else {
        (StatusCode::SERVICE_UNAVAILABLE, "not ready")
    }
}

// Watches the two readiness channels and logs whenever either component or the
// combined ready state transitions.  Runs as a background task for the lifetime
// of the process.
async fn watch_readiness(
    mut nats_ready_rx: tokio::sync::watch::Receiver<bool>,
    mut synced_rx: tokio::sync::watch::Receiver<bool>,
    sync_label: &'static str,
) {
    let mut prev_nats = *nats_ready_rx.borrow();
    let mut prev_synced = *synced_rx.borrow();
    let mut was_ready = prev_nats && prev_synced;
    loop {
        tokio::select! {
            result = nats_ready_rx.changed() => {
                if result.is_err() { return; }
                let nats_ready = *nats_ready_rx.borrow_and_update();
                if nats_ready != prev_nats {
                    if nats_ready {
                        tracing::info!("NATS ready");
                    } else {
                        tracing::warn!("NATS not ready");
                    }
                    prev_nats = nats_ready;
                }
            }
            result = synced_rx.changed() => {
                if result.is_err() { return; }
                let synced = *synced_rx.borrow_and_update();
                if synced != prev_synced {
                    if synced {
                        tracing::info!("{sync_label} synced");
                    } else {
                        tracing::warn!("{sync_label} not synced");
                    }
                    prev_synced = synced;
                }
            }
        }
        let is_ready = prev_nats && prev_synced;
        if is_ready != was_ready {
            if is_ready {
                tracing::info!("readiness: ready");
            } else {
                tracing::warn!(nats_ready = prev_nats, synced = prev_synced, "readiness: not ready");
            }
            was_ready = is_ready;
        }
    }
}

