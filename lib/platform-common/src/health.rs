use axum::{extract::State, http::StatusCode};

#[derive(Clone)]
pub struct ReadyState {
    pub nats_ready_rx: tokio::sync::watch::Receiver<bool>,
    pub synced_rx: tokio::sync::watch::Receiver<bool>,
}

pub async fn healthz_handler() -> &'static str {
    "OK"
}

pub async fn readyz_handler(
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

/// Logs readiness transitions. Runs as a background task for the lifetime of the process.
pub async fn watch_readiness(
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
