use anyhow::{Context, Result};
use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
    time::Duration,
};
use tokio_stream::wrappers::ReceiverStream;

pub mod proto {
    tonic::include_proto!("configsync.v1");
}

pub use proto::ApplicationConfig;

use proto::{
    FullConfigRequest, IncrementalUpdateAck, config_sync_client::ConfigSyncClient as TonicClient,
};

/// Shared configuration state updated on each full config response or incremental update.
#[derive(Default, Clone)]
pub struct AppConfig {
    pub version: String,
    pub applications: HashMap<String, ApplicationConfig>,
    pub timestamp: i64,
}

/// Connects to the wp-operator gRPC endpoint and keeps the shared `state` up
/// to date indefinitely. On any transport error the function reconnects,
/// re-fetches the full config, and reopens the incremental stream.
///
/// The function never returns in the happy path; cancel the containing task
/// to stop it.
pub async fn sync(endpoint: String, host_id: String, state: Arc<RwLock<AppConfig>>) {
    loop {
        if let Err(err) = sync_once(endpoint.clone(), host_id.clone(), Arc::clone(&state)).await {
            tracing::error!("config sync error: {err:#}");
        }
        tokio::time::sleep(Duration::from_secs(5)).await;
    }
}

/// Runs one full connect → full-config → incremental-stream cycle.
async fn sync_once(endpoint: String, host_id: String, state: Arc<RwLock<AppConfig>>) -> Result<()> {
    let mut client = TonicClient::connect(endpoint)
        .await
        .context("failed to connect to wp-operator gRPC endpoint")?;

    let config = request_full_config(&mut client, host_id.clone())
        .await
        .context("initial full config request failed")?;

    *state.write().expect("app config state lock poisoned") = config;

    run_incremental_stream(&mut client, host_id, state).await
}

async fn request_full_config(
    client: &mut TonicClient<tonic::transport::Channel>,
    host_id: String,
) -> Result<AppConfig> {
    let response = client
        .request_full_config(FullConfigRequest {
            host_id,
            last_ack_timestamp: None,
        })
        .await
        .context("RequestFullConfig RPC failed")?
        .into_inner();

    if !response.success {
        anyhow::bail!("RequestFullConfig returned failure: {}", response.message);
    }

    let full = response
        .config
        .context("RequestFullConfig returned no config")?;

    Ok(AppConfig {
        version: full.version,
        applications: full
            .applications
            .into_iter()
            .map(|app| (format!("{}/{}", app.namespace, app.name), app))
            .collect(),
        timestamp: full.timestamp,
    })
}

/// One ack per incremental update; operator sends at most one in-flight update
/// before receiving an ack, so a small buffer is sufficient.
const ACK_CHANNEL_BUFFER: usize = 32;

/// Opens the bidirectional `PushIncrementalUpdate` stream and processes
/// updates until the stream closes or an error occurs.
async fn run_incremental_stream(
    client: &mut TonicClient<tonic::transport::Channel>,
    host_id: String,
    state: Arc<RwLock<AppConfig>>,
) -> Result<()> {
    let (tx, rx) = tokio::sync::mpsc::channel::<IncrementalUpdateAck>(ACK_CHANNEL_BUFFER);
    let ack_stream = ReceiverStream::new(rx);

    let mut stream = client
        .push_incremental_update(ack_stream)
        .await
        .context("PushIncrementalUpdate RPC failed")?
        .into_inner();

    while let Some(msg) = stream
        .message()
        .await
        .context("incremental update stream receive failed")?
    {
        let Some(config) = msg.incremental_config else {
            tracing::warn!("received IncrementalUpdateRequest with no config payload; skipping");
            continue;
        };

        let version = config.version.clone();
        let (success, message) = apply_incremental(&state, config);

        let ack = IncrementalUpdateAck {
            host_id: host_id.clone(),
            version_applied: version,
            success,
            message,
        };

        if tx.send(ack).await.is_err() {
            break;
        }

        if !success {
            // The operator expects us to close the stream and re-request the
            // full config when we fail to apply a delta.
            return Ok(());
        }
    }

    Ok(())
}

fn apply_incremental(
    state: &Arc<RwLock<AppConfig>>,
    config: proto::IncrementalConfig,
) -> (bool, String) {
    let mut guard = match state.write() {
        Ok(g) => g,
        Err(err) => {
            let msg = format!("state lock poisoned: {err}");
            tracing::error!("{msg}");
            return (false, msg);
        }
    };

    for update in config.updates {
        let Some(app) = update.app_config else {
            tracing::warn!("received AppUpdate with no app_config; skipping");
            continue;
        };
        let key = format!("{}/{}", app.namespace, app.name);
        if update.delete {
            guard.applications.remove(&key);
            tracing::info!(%key, "application config removed");
        } else {
            guard.applications.insert(key.clone(), app);
            tracing::info!(%key, "application config updated");
        }
    }

    guard.version = config.version;
    guard.timestamp = config.timestamp;

    (true, String::new())
}
