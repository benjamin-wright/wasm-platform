mod config;
mod http_server;
mod nats;
mod route_sync;
mod route_table;

use anyhow::Result;
use axum::{Router, extract::State, http::StatusCode, routing::get};
use http_server::GatewayState;
use route_table::RouteTable;
use std::{path::PathBuf, sync::Arc};

// ── Entry point ───────────────────────────────────────────────────────────────

#[tokio::main]
async fn main() -> Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(tracing_subscriber::EnvFilter::from_default_env())
        .init();

    let cfg = config::Config::from_env()?;

    // NATS_CREDENTIALS_PATH must point to a directory containing NATS_USERNAME,
    // NATS_PASSWORD, NATS_HOST, and NATS_PORT files (Kubernetes secret volume
    // mount layout).  The manager re-reads this directory on every connection
    // attempt so that rotated credentials are picked up without pod restarts.
    let credentials_path = std::env::var("NATS_CREDENTIALS_PATH")
        .map(PathBuf::from)
        .map_err(|_| anyhow::anyhow!("NATS_CREDENTIALS_PATH environment variable is required"))?;

    tracing::info!("gateway starting");

    // Readiness state — the pod is ready only when both NATS and the route
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

    let table = RouteTable::new();

    // Route sync loop — fetches a full route snapshot on startup then maintains
    // the incremental update stream, reconnecting automatically on failure.
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

    // /healthz — liveness probe: always 200 while the process is alive.
    // /readyz  — readiness probe: 200 only when NATS is connected AND the
    //            route sync loop has received at least one full snapshot.
    let ready_state = ReadyState { nats_ready_rx, synced_rx };
    let app = Router::new()
        .route("/healthz", get(healthz_handler))
        .route("/readyz", get(readyz_handler).with_state(ready_state))
        .merge(http_server::build_router(Arc::clone(&state)));

    let addr = format!("0.0.0.0:{}", cfg.http_port);
    let listener = tokio::net::TcpListener::bind(&addr).await?;
    tracing::info!(%addr, "HTTP server listening");
    axum::serve(listener, app).await?;

    Ok(())
}

// ── Health probes ─────────────────────────────────────────────────────────────

#[derive(Clone)]
struct ReadyState {
    nats_ready_rx: tokio::sync::watch::Receiver<bool>,
    synced_rx: tokio::sync::watch::Receiver<bool>,
}

async fn healthz_handler() -> &'static str {
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
