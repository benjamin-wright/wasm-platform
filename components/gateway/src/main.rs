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
    tokio::spawn(watch_readiness(
        nats_ready_rx.clone(),
        synced_rx.clone(),
        "route sync",
    ));
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
