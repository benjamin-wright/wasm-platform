mod config;
mod config_sync;
mod dispatch_metrics;
mod host_kv;
mod host_log;
mod host_messaging;
mod host_metrics;
mod host_sql;
mod invocation;
mod invocation_supervisor;
mod metrics;
mod module_cache;
mod modules;
mod nats;
mod oci;
mod preparation_metrics;
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
use invocation_supervisor::{DispatchContext, InvocationSupervisor};
use metrics::MetricsRegistry;
use modules::ModuleRegistry;
use platform_common::health::{self, ReadyState};
use platform_common::nats_client::NatsConnectionInfo;
use runtime::RuntimeState;
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
    let workload_token = platform_common::identity::ProjectedTokenReader::platform();
    workload_token.validate().await?;

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
        oci::OciSource::from_env()?,
    )?;

    tracing::info!("execution-host starting");

    let addr = std::env::var("CONFIG_SYNC_ADDR")
        .map_err(|_| anyhow::anyhow!("CONFIG_SYNC_ADDR environment variable is required"))?;
    let host_id = std::env::var("HOSTNAME").unwrap_or_else(|_| "unknown".to_string());

    let app_registry = AppRegistry::new();
    let (topics_tx, topics_rx) = tokio::sync::watch::channel(Vec::<String>::new());
    const QUEUE_CAPACITY: usize = 256;
    let (msg_tx, msg_rx) = tokio::sync::mpsc::channel::<nats::InvocationMessage>(QUEUE_CAPACITY);

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
        workload_token,
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
        synced_rx: synced_rx.clone(),
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

    let mut supervisor = InvocationSupervisor::new(
        max_concurrent,
        metrics_registry.dispatch(),
        msg_rx,
        subscription_manager,
    );
    let dispatch = supervisor.run(
        DispatchContext {
            state: Arc::clone(&state),
            app_registry,
            module_registry,
            metrics_registry,
            client_rx,
            synced_rx,
        },
        shutdown_tx.subscribe(),
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
