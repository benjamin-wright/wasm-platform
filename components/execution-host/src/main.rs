mod config;
mod config_sync;
mod host_kv;
mod host_log;
mod host_messaging;
mod module_cache;
mod modules;
mod nats;
mod oci;
mod runtime;

use anyhow::Result;
use axum::{Router, routing::get};
use config::AppRegistry;
use modules::ModuleRegistry;
use platform_common::health::{self, ReadyState};
use platform_common::http_types::{HttpRequestPayload, HttpResponsePayload};
use runtime::{RuntimeState, invoke_on_message, invoke_on_request};
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

    let redis_client = match std::env::var("REDIS_URL") {
        Ok(url) => {
            tracing::info!(%url, "connecting to Redis");
            Some(
                redis::Client::open(url.as_str())
                    .map_err(|e| anyhow::anyhow!("invalid REDIS_URL: {e}"))?,
            )
        }
        Err(_) => {
            tracing::warn!("REDIS_URL not set; kv host functions will be unavailable");
            None
        }
    };

    let state = Arc::new(RuntimeState::new(engine.clone(), redis_client)?);

    let cache_addr = std::env::var("MODULE_CACHE_ADDR")
        .map_err(|_| anyhow::anyhow!("MODULE_CACHE_ADDR environment variable is required"))?;

    let module_registry = ModuleRegistry::new(cache_addr, engine);

    tracing::info!("execution-host starting");

    let addr = std::env::var("CONFIG_SYNC_ADDR")
        .map_err(|_| anyhow::anyhow!("CONFIG_SYNC_ADDR environment variable is required"))?;
    let host_id = std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown".to_string());

    let credentials_path = std::env::var("NATS_CREDENTIALS_PATH")
        .map(PathBuf::from)
        .map_err(|_| anyhow::anyhow!("NATS_CREDENTIALS_PATH environment variable is required"))?;

    let app_registry = AppRegistry::new();
    let (topics_tx, topics_rx) = tokio::sync::watch::channel(Vec::<String>::new());
    let (msg_tx, msg_rx) = tokio::sync::mpsc::channel::<async_nats::Message>(256);

    let (nats_ready_tx, nats_ready_rx) = tokio::sync::watch::channel(false);
    let (synced_tx, synced_rx) = tokio::sync::watch::channel(false);

    let (client_tx, client_rx) = tokio::sync::watch::channel::<Option<async_nats::Client>>(None);

    let (shutdown_tx, _) = tokio::sync::broadcast::channel::<()>(1);

    let shutdown_tx_for_sigterm = shutdown_tx.clone();
    tokio::spawn(async move {
        tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate())
            .expect("failed to install SIGTERM handler")
            .recv()
            .await;
        tracing::info!("received SIGTERM; initiating graceful shutdown");
        let _ = shutdown_tx_for_sigterm.send(());
    });

    tokio::spawn(nats::run_nats_manager(
        credentials_path,
        client_tx,
        nats_ready_tx,
    ));

    tokio::spawn(config_sync::run_config_sync_loop(
        addr,
        host_id,
        app_registry.clone(),
        module_registry.clone(),
        topics_tx,
        synced_tx,
    ));
    tokio::spawn(nats::manage_nats_subscriptions(client_rx.clone(), topics_rx, msg_tx, shutdown_tx.subscribe()));

    let max_concurrent = std::env::var("MAX_CONCURRENT_INVOCATIONS")
        .ok()
        .and_then(|v| v.parse::<usize>().ok())
        .unwrap_or(64);

    tokio::spawn(health::watch_readiness(
        nats_ready_rx.clone(),
        synced_rx.clone(),
        "config sync",
    ));
    let ready_state = ReadyState { nats_ready_rx, synced_rx };
    let health_app = Router::new()
        .route("/healthz", get(health::healthz_handler))
        .route("/readyz", get(health::readyz_handler))
        .with_state(ready_state);
    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000").await?;
    let health_server = axum::serve(listener, health_app);

    tokio::select! {
        result = health_server => {
            result?;
        }
        _ = process_nats_messages(msg_rx, Arc::clone(&state), app_registry, module_registry, client_rx, shutdown_tx.subscribe(), max_concurrent) => {}
    }

    Ok(())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// Receives NATS messages from all per-topic subscribers and dispatches them
// concurrently up to max_concurrent in-flight invocations.  On shutdown, drains
// all in-flight tasks before returning.
async fn process_nats_messages(
    mut msg_rx: tokio::sync::mpsc::Receiver<async_nats::Message>,
    state: Arc<RuntimeState>,
    app_registry: AppRegistry,
    module_registry: ModuleRegistry,
    client_rx: tokio::sync::watch::Receiver<Option<async_nats::Client>>,
    mut shutdown_rx: tokio::sync::broadcast::Receiver<()>,
    max_concurrent: usize,
) {
    let semaphore = Arc::new(tokio::sync::Semaphore::new(max_concurrent));
    let mut join_set = tokio::task::JoinSet::new();

    loop {
        let message = tokio::select! {
            msg = msg_rx.recv() => {
                match msg {
                    Some(m) => m,
                    None => break,
                }
            }
            _ = shutdown_rx.recv() => break,
        };

        let permit = match Arc::clone(&semaphore).acquire_owned().await {
            Ok(p) => p,
            Err(_) => break,
        };

        let subject = message.subject.to_string();
        let fn_entry = match app_registry.get_by_topic(&subject) {
            Ok(Some(entry)) => entry,
            Ok(None) => {
                tracing::warn!(%subject, "received message for unknown topic; dropping");
                continue;
            }
            Err(err) => {
                tracing::error!(%subject, "app registry error: {err:#}");
                continue;
            }
        };

        let component = match module_registry.get(&fn_entry.app_namespace, &fn_entry.app_name, &fn_entry.function_name) {
            Ok(Some(c)) => c,
            Ok(None) => {
                tracing::warn!(
                    namespace = %fn_entry.app_namespace,
                    app_name = %fn_entry.app_name,
                    function_name = %fn_entry.function_name,
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
        let kv_prefix = fn_entry
            .key_value
            .as_ref()
            .map(|kv| kv.prefix.clone())
            .unwrap_or_default();
        let app_name = fn_entry.app_name.clone();
        let app_namespace = fn_entry.app_namespace.clone();
        let function_name = fn_entry.function_name.clone();
        let world_type = fn_entry.world_type;
        let nats_for_invoke = client_snapshot.clone();

        join_set.spawn(async move {
            let _permit = permit;
            let reply = message.reply.clone();
            let payload = message.payload.to_vec();

            // WASM execution is CPU-bound; run it on the blocking thread
            // pool so the async runtime stays responsive.
            let result = match world_type {
                config::configsync::WorldType::Message => {
                    tokio::task::spawn_blocking(move || {
                        invoke_on_message(&state, &component, &payload, kv_prefix, nats_for_invoke, app_name, app_namespace, function_name)
                    })
                    .await
                }
                config::configsync::WorldType::Http => {
                    tokio::task::spawn_blocking(move || {
                        let request: HttpRequestPayload =
                            serde_json::from_slice(&payload).map_err(|e| {
                                anyhow::anyhow!("failed to decode HTTP request payload: {e}")
                            })?;
                        let response = invoke_on_request(&state, &component, request, kv_prefix, nats_for_invoke, app_name, app_namespace, function_name)?;
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
                    tracing::error!("invocation failed: {err:#}");
                    // For HTTP functions, send a 500 back so the gateway can return
                    // a proper error response instead of timing out.
                    if let (Some(reply_subject), Some(client)) = (reply, client_snapshot) {
                        let error_response = HttpResponsePayload {
                            status: 500,
                            headers: vec![("content-type".to_string(), "text/plain".to_string())],
                            body: Some(format!("internal error: {err:#}").into_bytes()),
                        };
                        if let Ok(bytes) = serde_json::to_vec(&error_response) {
                            let _ = client.publish(reply_subject, bytes.into()).await;
                        }
                    }
                }
                Err(join_err) => {
                    tracing::error!("spawn_blocking panicked: {join_err}");
                    if let (Some(reply_subject), Some(client)) = (reply, client_snapshot) {
                        let error_response = HttpResponsePayload {
                            status: 500,
                            headers: vec![("content-type".to_string(), "text/plain".to_string())],
                            body: Some(b"internal error: execution panicked".to_vec()),
                        };
                        if let Ok(bytes) = serde_json::to_vec(&error_response) {
                            let _ = client.publish(reply_subject, bytes.into()).await;
                        }
                    }
                }
            }
        });
    }

    tracing::info!("message channel closed; draining in-flight invocations");
    join_set.join_all().await;
    tracing::info!("drain complete; exiting");
}



