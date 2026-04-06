use std::time::Duration;

use anyhow::Result;

use crate::{
    config::{
        AppRegistry,
        configsync::{FullConfigRequest, IncrementalUpdateAck, config_sync_client::ConfigSyncClient},
    },
    modules::ModuleRegistry,
};

// Loops forever: fetches a full config snapshot then maintains the incremental
// update stream.  On any error or clean stream close, backs off and retries.
// `synced_tx` is set to `true` after the first successful full snapshot and
// back to `false` while reconnecting, so readiness reflects operator reachability.
pub async fn run_config_sync_loop(
    addr: String,
    host_id: String,
    registry: AppRegistry,
    modules: ModuleRegistry,
    topics_tx: tokio::sync::watch::Sender<Vec<String>>,
    synced_tx: tokio::sync::watch::Sender<bool>,
) {
    let mut backoff = Duration::from_secs(1);
    loop {
        match run_config_sync(&addr, &host_id, &registry, &modules, &topics_tx, &synced_tx).await {
            Ok(()) => {
                tracing::warn!("config sync stream closed; reconnecting");
                backoff = Duration::from_secs(1);
            }
            Err(err) => {
                tracing::warn!("config sync error: {err:#}; reconnecting in {backoff:?}");
                let _ = synced_tx.send(false);
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        }
        tokio::time::sleep(backoff).await;
    }
}

// Fetches a full config snapshot then drives a single incremental update stream
// session until it closes or errors.
async fn run_config_sync(
    addr: &str,
    host_id: &str,
    registry: &AppRegistry,
    modules: &ModuleRegistry,
    topics_tx: &tokio::sync::watch::Sender<Vec<String>>,
    synced_tx: &tokio::sync::watch::Sender<bool>,
) -> Result<()> {
    fetch_full_config(addr.to_string(), host_id.to_string(), registry, modules).await?;
    topics_tx.send(registry.topics()?).ok();
    let _ = synced_tx.send(true);

    tracing::info!("opening incremental update stream");
    let mut client = ConfigSyncClient::connect(addr.to_string()).await?;
    tracing::info!("gRPC client connected for config updates");

    // Bridge tokio mpsc → futures Stream so tonic can consume acks.
    let (ack_tx, ack_rx) = tokio::sync::mpsc::channel::<IncrementalUpdateAck>(16);
    let ack_stream = futures_util::stream::unfold(ack_rx, |mut rx| async move {
        tracing::debug!("ack_stream: waiting for next ack from mpsc");
        let item = rx.recv().await;
        tracing::debug!(has_item = item.is_some(), "ack_stream: polled");
        item.map(|ack| (ack, rx))
    });

    tracing::info!("calling push_incremental_update RPC");
    let mut update_stream = client
        .push_incremental_update(ack_stream)
        .await?
        .into_inner();
    tracing::info!("push_incremental_update stream opened");

    // Send an initial ack immediately so the server can identify and register
    // this host before any updates are broadcast. Without this the server
    // blocks on Recv() waiting for an id while we block on message() waiting
    // for an update — a deadlock that prevents incremental config delivery.
    let initial_ack = IncrementalUpdateAck {
        host_id: host_id.to_string(),
        version_applied: String::new(),
        success: true,
        message: String::new(),
    };
    tracing::info!("sending initial host ack");
    if ack_tx.send(initial_ack).await.is_err() {
        tracing::warn!("failed to send initial host ack — channel closed");
        return Ok(());
    }
    tracing::info!("initial host ack sent to mpsc channel");

    tracing::info!("waiting for first incremental update message");
    while let Some(request) = update_stream.message().await? {
        if let Some(incremental) = request.incremental_config {
            let version = incremental.version.clone();
            let update_count = incremental.updates.len();
            let (upserted, deleted) = registry.apply_incremental(incremental.updates)?;
            topics_tx.send(registry.topics()?).ok();
            tracing::debug!(version, update_count, "incremental config applied");

            apply_module_changes(modules, upserted, deleted).await;

            let ack = IncrementalUpdateAck {
                host_id: host_id.to_string(),
                version_applied: version,
                success: true,
                message: String::new(),
            };
            if ack_tx.send(ack).await.is_err() {
                break;
            }
        }
    }
    Ok(())
}

// Connects to the operator's gRPC endpoint, requests a full config snapshot,
// and applies it to the registry.  Returns an error if the connection or RPC
// fails so that the process exits rather than silently running unconfigured.
async fn fetch_full_config(
    addr: String,
    host_id: String,
    registry: &AppRegistry,
    modules: &ModuleRegistry,
) -> Result<()> {
    tracing::info!(%addr, "connecting to operator for full config");
    let mut client = ConfigSyncClient::connect(addr).await?;
    let response = client
        .request_full_config(FullConfigRequest {
            host_id,
            last_ack_timestamp: None,
        })
        .await?
        .into_inner();
    if let Some(full) = response.config {
        let app_count = full.applications.len();
        let (upserted, deleted) = registry.apply_full_config(full)?;
        tracing::info!(app_count, "full config applied");
        apply_module_changes(modules, upserted, deleted).await;
    } else {
        tracing::warn!("operator returned empty full config response");
    }
    Ok(())
}

// Spawns module load/evict tasks for all changes from a config update.
// Load failures are logged but do not abort the config sync loop.
async fn apply_module_changes(
    modules: &ModuleRegistry,
    upserted: Vec<crate::config::ApplicationConfig>,
    deleted: Vec<(String, String)>,
) {
    for app in upserted {
        let m = modules.clone();
        tokio::spawn(async move {
            if let Err(err) = m.load(&app.namespace, &app.name, &app.module_ref).await {
                tracing::error!(
                    namespace = %app.namespace,
                    name = %app.name,
                    "failed to load module: {err:#}"
                );
            }
        });
    }
    for (namespace, name) in deleted {
        if let Err(err) = modules.remove(&namespace, &name) {
            tracing::warn!(namespace, name, "failed to evict module: {err:#}");
        }
    }
}
