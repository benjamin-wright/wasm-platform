mod config;
mod http_server;
mod listener;
mod metrics;
mod nats;
mod route_sync;
mod route_table;
mod supervisor;

#[cfg(test)]
mod supervisor_tests;

use anyhow::Result;
use axum::{Router, extract::State, http::StatusCode, response::IntoResponse, routing::get};
use listener::BoundedListener;
use metrics::MetricsRegistry;
use nats::NatsBroker;
use platform_common::{
    health::{self, ReadyState},
    nats_client::NatsConnectionInfo,
};
use route_table::RouteTable;
use std::sync::Arc;
use supervisor::RequestSupervisor;
use tracing_subscriber::prelude::*;

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

    let cfg = config::Config::from_env()?;
    let workload_token = platform_common::identity::ProjectedTokenReader::platform();
    workload_token.validate().await?;

    let nats_conn_info = read_nats_credentials()?;

    tracing::info!("gateway starting");

    let (nats_ready_tx, nats_ready_rx) = tokio::sync::watch::channel(false);
    let (synced_tx, synced_rx) = tokio::sync::watch::channel(false);

    let broker = Arc::new(NatsBroker::new(
        nats_conn_info,
        nats_ready_tx,
        cfg.requests.in_flight,
    ));

    let table = RouteTable::new();

    tokio::spawn(route_sync::run_route_sync_loop(
        cfg.operator_addr.clone(),
        cfg.gateway_id.clone(),
        workload_token,
        table.clone(),
        synced_tx,
    ));

    let header_timeout = cfg.requests.body_read_timeout;
    let supervisor = Arc::new(RequestSupervisor::new(
        table,
        broker,
        cfg.requests,
        synced_rx.clone(),
    ));
    let ready_state = ReadyState {
        nats_ready_rx: nats_ready_rx.clone(),
        synced_rx: synced_rx.clone(),
    };
    let metrics = MetricsRegistry::new(Arc::clone(&supervisor))?;
    let metrics_app = Router::new()
        .route("/metrics", get(metrics_handler))
        .with_state(metrics)
        .route("/healthz", get(health::healthz_handler))
        .route(
            "/readyz",
            get(health::readyz_handler).with_state(ready_state.clone()),
        );
    let metrics_listener = tokio::net::TcpListener::bind("0.0.0.0:9090").await?;
    let metrics_listener =
        BoundedListener::new(metrics_listener, Arc::new(tokio::sync::Semaphore::new(8)));
    tokio::spawn(async move {
        http_server::serve_http(metrics_listener, metrics_app, header_timeout).await;
    });

    tokio::spawn(health::watch_readiness(
        nats_ready_rx.clone(),
        synced_rx.clone(),
        "route sync",
    ));
    let app = Router::new()
        .route("/healthz", get(health::healthz_handler))
        .route(
            "/readyz",
            get(health::readyz_handler).with_state(ready_state),
        )
        .merge(http_server::build_router(Arc::clone(&supervisor)));

    let addr = format!("0.0.0.0:{}", cfg.http_port);
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    let listener = BoundedListener::new(listener, supervisor.connection_capacity());
    tracing::info!(%addr, "HTTP server listening");
    http_server::serve_http(listener, app, header_timeout).await;

    Ok(())
}

async fn metrics_handler(State(metrics): State<MetricsRegistry>) -> axum::response::Response {
    match metrics.render() {
        Ok(body) => (
            [(
                axum::http::header::CONTENT_TYPE,
                "text/plain; version=0.0.4",
            )],
            body,
        )
            .into_response(),
        Err(_) => StatusCode::INTERNAL_SERVER_ERROR.into_response(),
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
