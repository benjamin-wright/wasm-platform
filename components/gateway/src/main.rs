mod config;
mod http_server;
mod nats;
mod route_sync;
mod route_table;

use anyhow::Result;
use axum::{Router, routing::get};
use http_server::GatewayState;
use platform_common::health::{self, ReadyState};
use route_table::RouteTable;
use std::{path::PathBuf, sync::Arc};

// ── Entry point ───────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let cfg = config::Config::from_env()?;

    let credentials_path = std::env::var("NATS_CREDENTIALS_PATH")
        .map(PathBuf::from)
        .map_err(|_| anyhow::anyhow!("NATS_CREDENTIALS_PATH environment variable is required"))?;

    tracing::info!("gateway starting");

    let (nats_ready_tx, nats_ready_rx) = tokio::sync::watch::channel(false);
    let (synced_tx, synced_rx) = tokio::sync::watch::channel(false);

    let (client_tx, client_rx) = tokio::sync::watch::channel::<Option<async_nats::Client>>(None);

    tokio::spawn(nats::run_nats_manager(
        credentials_path,
        client_tx,
        nats_ready_tx,
    ));

    let table = RouteTable::new();

    tokio::spawn(route_sync::run_route_sync_loop(
        cfg.operator_addr.clone(),
        cfg.gateway_id.clone(),
        table.clone(),
        synced_tx,
    ));

    let state = Arc::new(GatewayState {
        table,
        nats: client_rx.clone(),
        timeout: std::time::Duration::from_secs(cfg.timeout_secs),
    });

    tokio::spawn(health::watch_readiness(
        nats_ready_rx.clone(),
        synced_rx.clone(),
        "route sync",
    ));
    let ready_state = ReadyState { nats_ready_rx, synced_rx };
    let app = Router::new()
        .route("/healthz", get(health::healthz_handler))
        .route("/readyz", get(health::readyz_handler).with_state(ready_state))
        .merge(http_server::build_router(Arc::clone(&state)));

    let addr = format!("0.0.0.0:{}", cfg.http_port);
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    tracing::info!(%addr, "HTTP server listening");
    axum::serve(listener, app).await?;

    Ok(())
}


