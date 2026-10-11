mod config;
mod config_sync;
mod dispatch_metrics;
mod host_kv;
mod host_log;
mod host_messaging;
mod host_metrics;
mod host_sql;
mod invocation;
mod metrics;
mod module_cache;
mod modules;
mod nats;
mod oci;
mod redis_client;
mod runtime;
mod sql_pool;

#[cfg(test)]
mod logging_tests;

#[cfg(test)]
mod dependency_logging_tests;

#[cfg(test)]
mod panic_logging_tests;

use anyhow::Result;
use axum::{Router, extract::State, response::IntoResponse, routing::get};
use config::AppRegistry;
use metrics::MetricsRegistry;
use modules::ModuleRegistry;
use platform_common::health::{self, ReadyState};
use platform_common::http_types::{HttpRequestPayload, HttpResponsePayload};
use platform_common::nats_client::NatsConnectionInfo;
use runtime::{InvocationContext, RuntimeState, invoke_on_message, invoke_on_request};
use sql_pool::SqlPoolMap;
use std::sync::{Arc, RwLock};
use tracing_subscriber::prelude::*;
use wasmtime::Engine;

// ── Entry point ───────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::registry()
        .with(tracing_subscriber::EnvFilter::from_default_env())
        .with(
            tracing_subscriber::fmt::layer().with_filter(tracing_subscriber::filter::filter_fn(
                platform_common::logging::is_platform_log,
            )),
        )
        .init();
    platform_common::logging::install_panic_hook();

    let mut wasm_config = wasmtime::Config::new();
    wasm_config.wasm_component_model(true);
    wasm_config.epoch_interruption(true);

    let fuel_limit: Option<u64> = std::env::var("WASM_FUEL_LIMIT")
        .ok()
        .and_then(|v| v.parse().ok());
    if let Some(limit) = fuel_limit {
        wasm_config.consume_fuel(true);
        tracing::info!(limit, "fuel metering enabled");
    }

    let memory_limit_bytes: usize = std::env::var("WASM_MEMORY_LIMIT_MB")
        .ok()
        .and_then(|v| v.parse::<usize>().ok())
        .unwrap_or(64)
        * 1024
        * 1024;

    let wasm_timeout_secs: u64 = std::env::var("WASM_TIMEOUT_SECS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(30);
    anyhow::ensure!(wasm_timeout_secs > 0, "WASM_TIMEOUT_SECS must be positive");
    let wasm_timeout = std::time::Duration::from_secs(wasm_timeout_secs);
    tracing::info!(secs = wasm_timeout_secs, "wall-clock timeout configured");

    let max_concurrent = std::env::var("MAX_CONCURRENT_INVOCATIONS")
        .ok()
        .and_then(|v| v.parse::<usize>().ok())
        .unwrap_or(64);
    anyhow::ensure!(
        max_concurrent > 0 && u32::try_from(max_concurrent).is_ok(),
        "MAX_CONCURRENT_INVOCATIONS must be positive and fit in u32"
    );

    let engine = Engine::new(&wasm_config)?;

    // Redis client is managed dynamically: configsync delivers the URL when
    // the operator provisions the RedisDatabase; the watcher task keeps
    // this shared cell up-to-date so invocations always use current creds.
    let redis_client: Arc<RwLock<Option<redis::Client>>> = Arc::new(RwLock::new(None));
    tracing::info!("Redis client will be configured via configsync");

    let metrics_registry = MetricsRegistry::new()?;

    let pg_pool_max: u32 = std::env::var("PG_POOL_MAX_CONNECTIONS")
        .ok()
        .and_then(|v| v.parse().ok())
        .unwrap_or(5);
    let sql_pools = SqlPoolMap::new(pg_pool_max);

    let state = Arc::new(RuntimeState::new(
        engine.clone(),
        Arc::clone(&redis_client),
        metrics_registry.clone(),
        Arc::clone(&sql_pools),
        fuel_limit,
        memory_limit_bytes,
    )?);

    let cache_addr = std::env::var("MODULE_CACHE_ADDR")
        .map_err(|_| anyhow::anyhow!("MODULE_CACHE_ADDR environment variable is required"))?;

    let module_registry = ModuleRegistry::new(
        cache_addr,
        engine,
        metrics_registry.clone(),
        fuel_limit.is_some(),
    );

    tracing::info!("execution-host starting");

    let addr = std::env::var("CONFIG_SYNC_ADDR")
        .map_err(|_| anyhow::anyhow!("CONFIG_SYNC_ADDR environment variable is required"))?;
    let host_id = std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown".to_string());

    let app_registry = AppRegistry::new();
    let (topics_tx, topics_rx) = tokio::sync::watch::channel(Vec::<String>::new());
    const QUEUE_CAPACITY: usize = 256;
    let (msg_tx, msg_rx) = tokio::sync::mpsc::channel::<nats::InvocationMessage>(QUEUE_CAPACITY);
    metrics_registry
        .dispatch()
        .configure(QUEUE_CAPACITY + 1, max_concurrent, max_concurrent);

    let (nats_ready_tx, nats_ready_rx) = tokio::sync::watch::channel(false);
    let (synced_tx, synced_rx) = tokio::sync::watch::channel(false);

    let (client_tx, client_rx) = tokio::sync::watch::channel::<Option<async_nats::Client>>(None);

    // Channels that configsync populates with infrastructure connection info
    // received from the operator's configsync gRPC stream.
    let (redis_url_tx, mut redis_url_rx) = tokio::sync::watch::channel::<Option<String>>(None);

    // NATS credentials are read once from the Secret volume mounted at
    // NATS_CREDENTIALS_PATH.  The watch channel is initialised with the
    // static value so run_nats_manager connects immediately on startup.
    let nats_conn_info = read_nats_credentials()?;
    let (_nats_conn_tx, nats_conn_rx) =
        tokio::sync::watch::channel::<Option<NatsConnectionInfo>>(Some(nats_conn_info));

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

    // Watch redis_url_rx and keep the shared redis_client cell up-to-date.
    let redis_client_for_watcher = Arc::clone(&redis_client);
    tokio::spawn(async move {
        loop {
            if redis_url_rx.changed().await.is_err() {
                break;
            }
            let url_opt = redis_url_rx.borrow_and_update().clone();
            let new_client = url_opt.and_then(|url| redis_client::configured_client(&url));
            if let Ok(mut guard) = redis_client_for_watcher.write() {
                *guard = new_client;
            }
        }
    });

    tokio::spawn(nats::run_nats_manager(
        nats_conn_rx,
        client_tx,
        nats_ready_tx,
    ));

    tokio::spawn(config_sync::run_config_sync_loop(
        addr,
        host_id,
        app_registry.clone(),
        module_registry.clone(),
        metrics_registry.clone(),
        topics_tx,
        synced_tx,
        sql_pools,
        redis_url_tx,
    ));
    let subscription_manager = tokio::spawn(nats::manage_nats_subscriptions(
        client_rx.clone(),
        topics_rx,
        msg_tx,
        shutdown_tx.subscribe(),
        wasm_timeout,
        metrics_registry.dispatch(),
    ));

    tokio::spawn(health::watch_readiness(
        nats_ready_rx.clone(),
        synced_rx.clone(),
        "config sync",
    ));
    let ready_state = ReadyState {
        nats_ready_rx,
        synced_rx,
    };
    let health_app = Router::new()
        .route("/healthz", get(health::healthz_handler))
        .route("/readyz", get(health::readyz_handler))
        .with_state(ready_state);
    let listener = tokio::net::TcpListener::bind("0.0.0.0:3000").await?;
    let health_server = axum::serve(listener, health_app);

    let metrics_app = Router::new()
        .route("/metrics", get(metrics_handler))
        .with_state(metrics_registry.clone());
    let metrics_listener = tokio::net::TcpListener::bind("0.0.0.0:9090").await?;
    tokio::spawn(async {
        if let Err(e) = axum::serve(metrics_listener, metrics_app).await {
            tracing::error!("metrics server error: {e}");
        }
    });

    let dispatch = process_nats_messages(
        msg_rx,
        DispatchContext {
            state: Arc::clone(&state),
            app_registry,
            module_registry,
            metrics_registry,
            client_rx,
        },
        shutdown_tx.subscribe(),
        max_concurrent,
        wasm_timeout,
        subscription_manager,
    );
    tokio::pin!(dispatch);
    tokio::select! {
        result = health_server => {
            let _ = shutdown_tx.send(());
            dispatch.await;
            result?;
        }
        _ = &mut dispatch => {}
    }

    Ok(())
}

// ── Helpers ───────────────────────────────────────────────────────────────────

struct DispatchContext {
    state: Arc<RuntimeState>,
    app_registry: AppRegistry,
    module_registry: ModuleRegistry,
    metrics_registry: MetricsRegistry,
    client_rx: tokio::sync::watch::Receiver<Option<async_nats::Client>>,
}

async fn process_nats_messages(
    mut msg_rx: tokio::sync::mpsc::Receiver<nats::InvocationMessage>,
    context: DispatchContext,
    mut shutdown_rx: tokio::sync::broadcast::Receiver<()>,
    max_concurrent: usize,
    wasm_timeout: std::time::Duration,
    subscription_manager: tokio::task::JoinHandle<()>,
) {
    let DispatchContext {
        state,
        app_registry,
        module_registry,
        metrics_registry,
        client_rx,
    } = context;
    let semaphore = Arc::new(tokio::sync::Semaphore::new(max_concurrent));
    let mut join_set = tokio::task::JoinSet::new();
    let dispatch_metrics = metrics_registry.dispatch();

    'dispatch: loop {
        let message = loop {
            tokio::select! {
                biased;
                _ = shutdown_rx.recv() => break 'dispatch,
                Some(result) = join_set.join_next(), if !join_set.is_empty() => {
                    reap_dispatch_task(result, &dispatch_metrics);
                }
                message = msg_rx.recv(), if join_set.len() < max_concurrent => {
                    match message {
                        Some(message) => break message,
                        None => break 'dispatch,
                    }
                }
            }
        };

        let nats::InvocationMessage {
            message,
            invocation,
            mut queued,
        } = message;
        let admission = {
            let admission = invocation.acquire_permit(Arc::clone(&semaphore));
            tokio::pin!(admission);
            loop {
                tokio::select! {
                    biased;
                    _ = shutdown_rx.recv() => break 'dispatch,
                    Some(result) = join_set.join_next(), if !join_set.is_empty() => {
                        reap_dispatch_task(result, &dispatch_metrics);
                    }
                    permit = &mut admission => break permit,
                }
            }
        };
        let permit = match admission {
            Ok(permit) => permit,
            Err(_) => {
                queued.set_outcome("expired");
                drop(queued);
                let client = client_rx.borrow().clone();
                let mut record = dispatch_metrics.task_started();
                join_set.spawn(async move {
                    record.set_outcome("timeout");
                    send_timeout_reply(&message, client).await;
                });
                continue;
            }
        };

        let subject = message.subject.to_string();
        let fn_entry = match app_registry.get_by_topic(&subject) {
            Ok(Some(entry)) => entry,
            Ok(None) => {
                queued.set_outcome("unknown_route");
                tracing::warn!(%subject, "received message for unknown topic; dropping");
                continue;
            }
            Err(err) => {
                queued.set_outcome("error");
                tracing::error!(%subject, "app registry error: {err:#}");
                continue;
            }
        };

        let component = match module_registry.get(
            &fn_entry.app_namespace,
            &fn_entry.app_name,
            &fn_entry.function_name,
        ) {
            Ok(Some(c)) => c,
            Ok(None) => {
                queued.set_outcome("module_unavailable");
                tracing::warn!(
                    namespace = %fn_entry.app_namespace,
                    app_name = %fn_entry.app_name,
                    function_name = %fn_entry.function_name,
                    "module not yet loaded; dropping message"
                );
                continue;
            }
            Err(err) => {
                queued.set_outcome("error");
                tracing::error!("module registry error: {err:#}");
                continue;
            }
        };
        queued.set_outcome("admitted");
        drop(queued);

        let state = Arc::clone(&state);
        // Snapshot the current client.  If NATS is mid-reconnect the snapshot
        // is None; replies will be silently dropped and the caller will time out.
        let client_snapshot = client_rx.borrow().clone();
        let app_name = fn_entry.app_name.clone();
        let app_namespace = fn_entry.app_namespace.clone();
        let function_name = fn_entry.function_name.clone();
        let world_type = fn_entry.world_type;
        let nats_for_invoke = client_snapshot.clone();

        let trigger = match world_type {
            config::configsync::WorldType::Http => "http",
            config::configsync::WorldType::Message => "topic",
        };
        metrics_registry.record_event(&app_name, &app_namespace, trigger);

        let mut record = dispatch_metrics.task_started();
        join_set.spawn(async move {
            let reply = message.reply.clone();
            let payload = message.payload.to_vec();
            let app_name_log = app_name.clone();
            let app_namespace_log = app_namespace.clone();
            let function_name_log = function_name.clone();
            let cancellation = invocation.clone();

            let result = invocation::run_blocking(invocation, permit, move |invocation| {
                tokio::runtime::Handle::current().block_on(async move {
                    invocation.check().map_err(anyhow::Error::msg)?;
                    let context = InvocationContext {
                        function: fn_entry,
                        nats_client: nats_for_invoke,
                        invocation,
                    };
                    match world_type {
                        config::configsync::WorldType::Message => {
                            invoke_on_message(&state, &component, &payload, context).await
                        }
                        config::configsync::WorldType::Http => {
                            let request: HttpRequestPayload = serde_json::from_slice(&payload)?;
                            let response = invoke_on_request(&state, &component, request, context).await?;
                            Ok(Some(serde_json::to_vec(&response)?))
                        }
                    }
                })
            }).await;

            record.set_outcome(match &result {
                Ok(_) if cancellation.check().is_err() => "timeout",
                Ok(Ok(_)) => "success",
                Ok(Err(_)) => "error",
                Err(error) if error.is_panic() => "panic",
                Err(_) => "cancelled",
            });

            tracing::info!(
                app_name = %app_name_log,
                app_namespace = %app_namespace_log,
                function_name = %function_name_log,
                cancelled = cancellation.check().is_err(),
                expired = tokio::time::Instant::now() >= cancellation.deadline(),
                "invocation stopped"
            );
            if result.is_ok() && cancellation.check().is_err() {
                tracing::error!(
                    app_name = %app_name_log,
                    app_namespace = %app_namespace_log,
                    timeout_secs = wasm_timeout.as_secs(),
                    "invocation timed out"
                );
                if let (Some(reply_subject), Some(client)) = (reply, client_snapshot) {
                    publish_reply(&client, reply_subject, HttpResponsePayload {
                        status: 504,
                        headers: vec![("content-type".to_owned(), "text/plain".to_owned())],
                        body: Some(b"invocation timed out".to_vec()),
                    }).await;
                }
                return;
            }

            match result {
                Ok(Ok(Some(response_body))) => {
                    if let Some(reply_subject) = reply {
                        if let Some(client) = client_snapshot {
                            if let Err(err) = tokio::time::timeout(std::time::Duration::from_secs(2), client.publish(reply_subject.clone(), response_body.into()))
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
                Ok(Err(_err)) => {
                    tracing::error!(
                        app_name = %app_name_log,
                        app_namespace = %app_namespace_log,
                        "invocation failed"
                    );
                    // For HTTP functions, send a 500 back so the gateway can return
                    // a proper error response instead of timing out.
                    if let (Some(reply_subject), Some(client)) = (reply, client_snapshot) {
                        let error_response = HttpResponsePayload {
                            status: 500,
                            headers: vec![("content-type".to_string(), "text/plain".to_string())],
                            body: Some(b"internal error: invocation failed".to_vec()),
                        };
                        if let Ok(bytes) = serde_json::to_vec(&error_response) {
                            let _ = tokio::time::timeout(std::time::Duration::from_secs(2), client.publish(reply_subject, bytes.into())).await;
                        }
                    }
                }
                Err(join_err) => {
                    tracing::error!(panicked = join_err.is_panic(), cancelled = join_err.is_cancelled(), "invocation task failed");
                    if let (Some(reply_subject), Some(client)) = (reply, client_snapshot) {
                        let error_response = HttpResponsePayload {
                            status: 500,
                            headers: vec![("content-type".to_string(), "text/plain".to_string())],
                            body: Some(b"internal error: execution panicked".to_vec()),
                        };
                        if let Ok(bytes) = serde_json::to_vec(&error_response) {
                            let _ = tokio::time::timeout(std::time::Duration::from_secs(2), client.publish(reply_subject, bytes.into())).await;
                        }
                    }
                }
            }
        });
    }

    dispatch_metrics.dispatch_stopped();
    msg_rx.close();
    if let Err(error) = subscription_manager.await {
        tracing::error!(
            panicked = error.is_panic(),
            cancelled = error.is_cancelled(),
            "NATS intake task failed"
        );
    }
    while let Ok(message) = msg_rx.try_recv() {
        message.invocation.cancel();
    }
    tracing::info!("dispatch stopped; draining in-flight invocations");
    while let Some(result) = join_set.join_next().await {
        reap_dispatch_task(result, &dispatch_metrics);
    }
    // An outer task failure cannot establish that its blocking execution has exited.
    if let Ok(limit) = u32::try_from(max_concurrent) {
        let _all_execution_permits = semaphore.acquire_many(limit).await;
    }
    dispatch_metrics.drain_finished();
    tracing::info!("drain complete; exiting");
}

fn reap_dispatch_task(
    result: Result<(), tokio::task::JoinError>,
    metrics: &dispatch_metrics::DispatchMetrics,
) {
    let outcome = match result {
        Ok(()) => "ok",
        Err(error) => {
            tracing::error!(
                panicked = error.is_panic(),
                cancelled = error.is_cancelled(),
                "dispatch task failed"
            );
            if error.is_panic() {
                "panic"
            } else {
                "cancelled"
            }
        }
    };
    metrics.task_reaped(outcome);
}

async fn send_timeout_reply(message: &async_nats::Message, client: Option<async_nats::Client>) {
    if let (Some(reply), Some(client)) = (message.reply.clone(), client) {
        publish_reply(
            &client,
            reply,
            HttpResponsePayload {
                status: 504,
                headers: vec![("content-type".to_owned(), "text/plain".to_owned())],
                body: Some(b"invocation timed out".to_vec()),
            },
        )
        .await;
    }
}

async fn publish_reply(
    client: &async_nats::Client,
    reply: async_nats::Subject,
    response: HttpResponsePayload,
) {
    if let Ok(bytes) = serde_json::to_vec(&response) {
        let _ = tokio::time::timeout(
            std::time::Duration::from_secs(2),
            client.publish(reply, bytes.into()),
        )
        .await;
    }
}

// ── Metrics endpoint ──────────────────────────────────────────────────────────

async fn metrics_handler(State(registry): State<MetricsRegistry>) -> impl IntoResponse {
    match registry.render() {
        Ok(body) => (
            axum::http::StatusCode::OK,
            [(
                axum::http::header::CONTENT_TYPE,
                "text/plain; version=0.0.4; charset=utf-8",
            )],
            body,
        )
            .into_response(),
        Err(err) => {
            tracing::error!("failed to render metrics: {err:#}");
            axum::http::StatusCode::INTERNAL_SERVER_ERROR.into_response()
        }
    }
}

/// Reads NATS connection credentials from the directory mounted at
/// `NATS_CREDENTIALS_PATH`.  The directory is a Kubernetes Secret volume where
/// each Secret key becomes a file: `NATS_HOST`, `NATS_PORT`, `NATS_USERNAME`,
/// `NATS_PASSWORD`.
fn read_nats_credentials() -> anyhow::Result<NatsConnectionInfo> {
    let dir = std::env::var("NATS_CREDENTIALS_PATH")
        .map_err(|_| anyhow::anyhow!("NATS_CREDENTIALS_PATH environment variable is required"))?;
    let read = |key: &str| -> anyhow::Result<String> {
        let path = std::path::Path::new(&dir).join(key);
        std::fs::read_to_string(&path)
            .map(|s| s.trim().to_string())
            .map_err(|e| anyhow::anyhow!("reading {}: {e}", path.display()))
    };
    let host = read("NATS_HOST")?;
    let port = read("NATS_PORT")?;
    let username = read("NATS_USERNAME")?;
    let password = read("NATS_PASSWORD")?;
    Ok(NatsConnectionInfo {
        url: format!("nats://{}:{}", host, port),
        username,
        password,
    })
}
