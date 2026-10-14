use std::sync::Arc;
use std::time::Duration;

use anyhow::Result;
use platform_common::identity::{
    ProjectedTokenReader, RPC_TIMEOUT, SYNC_SESSION_TIMEOUT, operator_channel,
};

use crate::sql_pool::SqlPoolMap;
use crate::{
    config::{
        AppRegistry,
        configsync::{
            FullConfigRequest, IncrementalUpdateAck, RedisConnectionConfig,
            config_sync_client::ConfigSyncClient,
        },
    },
    metrics::MetricsRegistry,
    modules::ModuleRegistry,
};

// Loops forever: fetches a full config snapshot then maintains the incremental
// update stream.  On any error or clean stream close, backs off and retries.
// `synced_tx` requires an authoritative snapshot and authenticated stream
// registration; reconnecting revokes admission and stops topic subscriptions.
#[allow(clippy::too_many_arguments)]
pub async fn run_config_sync_loop(
    addr: String,
    host_id: String,
    token: ProjectedTokenReader,
    registry: AppRegistry,
    modules: ModuleRegistry,
    metrics: MetricsRegistry,
    topics_tx: tokio::sync::watch::Sender<Vec<String>>,
    synced_tx: tokio::sync::watch::Sender<bool>,
    sql_pools: Arc<SqlPoolMap>,
    redis_tx: tokio::sync::watch::Sender<Option<String>>,
) {
    let mut backoff = Duration::from_secs(1);
    loop {
        let result = tokio::time::timeout(
            SYNC_SESSION_TIMEOUT,
            run_config_sync(
                &addr, &host_id, &token, &registry, &modules, &metrics, &topics_tx, &synced_tx,
                &sql_pools, &redis_tx,
            ),
        )
        .await
        .unwrap_or_else(|_| Err(anyhow::anyhow!("config sync session expired")));
        if *synced_tx.borrow() {
            backoff = Duration::from_secs(1);
        }
        let _ = synced_tx.send(false);
        let _ = topics_tx.send(Vec::new());
        match result {
            Ok(()) => {
                tracing::warn!("config sync stream closed; reconnecting");
                backoff = Duration::from_secs(1);
            }
            Err(err) => {
                tracing::warn!(
                    error = %err,
                    retry_after_secs = backoff.as_secs(),
                    "config sync failed; reconnecting"
                );
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        }
        tokio::time::sleep(backoff).await;
    }
}

// Fetches a full config snapshot then drives a single incremental update stream
// session until it closes or errors.
#[allow(clippy::too_many_arguments)]
async fn run_config_sync(
    addr: &str,
    host_id: &str,
    token: &ProjectedTokenReader,
    registry: &AppRegistry,
    modules: &ModuleRegistry,
    metrics: &MetricsRegistry,
    topics_tx: &tokio::sync::watch::Sender<Vec<String>>,
    synced_tx: &tokio::sync::watch::Sender<bool>,
    sql_pools: &Arc<SqlPoolMap>,
    redis_tx: &tokio::sync::watch::Sender<Option<String>>,
) -> Result<()> {
    fetch_full_config(
        addr.to_string(),
        host_id.to_string(),
        token,
        registry,
        modules,
        sql_pools,
        redis_tx,
    )
    .await?;
    if metrics
        .sync_user_metrics(registry.all_app_metric_defs()?)
        .is_err()
    {
        tracing::warn!(
            stage = "full_config",
            error_kind = "metric_definitions",
            "failed to sync user metrics"
        );
    }
    topics_tx.send(registry.topics()?).ok();

    tracing::info!("opening incremental update stream");
    let mut client = operator_channel(addr)
        .await
        .map(ConfigSyncClient::new)
        .map_err(|_| anyhow::anyhow!("config update connection failed"))?;
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
        .push_incremental_update(token.grpc_request(ack_stream, SYNC_SESSION_TIMEOUT).await?)
        .await
        .map_err(|error| anyhow::anyhow!("config update RPC failed ({:?})", error.code()))?
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
    let _ = synced_tx.send(true);

    tracing::info!("waiting for first incremental update message");
    while let Some(request) = update_stream
        .message()
        .await
        .map_err(|error| anyhow::anyhow!("config update stream failed ({:?})", error.code()))?
    {
        if let Some(incremental) = request.incremental_config {
            let version = incremental.version.clone();
            let update_count = incremental.updates.len();
            apply_redis_config(&incremental.redis, redis_tx);
            let diff = registry.apply_incremental(incremental)?;
            if metrics
                .sync_user_metrics(registry.all_app_metric_defs()?)
                .is_err()
            {
                tracing::warn!(
                    stage = "incremental_config",
                    error_kind = "metric_definitions",
                    "failed to sync user metrics"
                );
            }
            topics_tx.send(registry.topics()?).ok();
            tracing::debug!(version, update_count, "incremental config applied");

            apply_module_changes(modules, diff.modules_to_load, diff.modules_to_evict).await?;
            apply_pool_changes(sql_pools, diff.pools_to_create, diff.pools_to_evict_apps).await;

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
    token: &ProjectedTokenReader,
    registry: &AppRegistry,
    modules: &ModuleRegistry,
    sql_pools: &Arc<SqlPoolMap>,
    redis_tx: &tokio::sync::watch::Sender<Option<String>>,
) -> Result<()> {
    tracing::info!("connecting to operator for full config");
    let mut client = operator_channel(&addr)
        .await
        .map(ConfigSyncClient::new)
        .map_err(|_| anyhow::anyhow!("config snapshot connection failed"))?;
    let response = client
        .request_full_config(
            token
                .grpc_request(
                    FullConfigRequest {
                        host_id,
                        last_ack_timestamp: None,
                    },
                    RPC_TIMEOUT,
                )
                .await?,
        )
        .await
        .map_err(|error| anyhow::anyhow!("config snapshot RPC failed ({:?})", error.code()))?
        .into_inner();
    if let Some(full) = response.config {
        apply_redis_config(&full.redis, redis_tx);
        let app_count = full.applications.len();
        let diff = registry.apply_full_config(full)?;
        tracing::info!(app_count, "full config applied");
        apply_module_changes(modules, diff.modules_to_load, diff.modules_to_evict).await?;
        apply_pool_changes(sql_pools, diff.pools_to_create, diff.pools_to_evict_apps).await;
    } else {
        return Err(anyhow::anyhow!(
            "operator returned uninitialized full config response"
        ));
    }
    Ok(())
}

// Applies Redis connection info from an incoming config to the redis_tx watch
// channel so the Redis client reconnects with the new credentials.
fn apply_redis_config(
    redis: &Option<RedisConnectionConfig>,
    redis_tx: &tokio::sync::watch::Sender<Option<String>>,
) {
    let redis_url = redis.as_ref().map(|r| r.url.clone());
    let _ = redis_tx.send(redis_url);
}

// Evicts stale pools synchronously, then spawns background tasks for each pool
// to create — matching the module loading pattern.
async fn apply_pool_changes(
    pools: &Arc<SqlPoolMap>,
    to_create: Vec<(String, String, String, String)>,
    to_evict_apps: Vec<(String, String)>,
) {
    for (ns, app) in to_evict_apps {
        pools.evict_app(&ns, &app).await;
    }
    for (ns, app, username, url) in to_create {
        let p = Arc::clone(pools);
        tokio::spawn(async move {
            p.ensure(ns, app, username, url).await;
        });
    }
}

// One config-sync caller waits for preparation capacity; failures force a full resync.
async fn apply_module_changes(
    modules: &ModuleRegistry,
    to_load: Vec<(String, String, String, String)>,
    to_evict: Vec<(String, String, String)>,
) -> Result<()> {
    for (namespace, app_name, fn_name) in to_evict {
        if let Err(err) = modules.remove(&namespace, &app_name, &fn_name) {
            tracing::warn!(
                namespace,
                app_name,
                fn_name,
                "failed to evict module: {err:#}"
            );
        }
    }
    for (namespace, app_name, fn_name, module_ref) in to_load {
        modules
            .submit_load(namespace, app_name, fn_name, module_ref)
            .await?;
    }
    Ok(())
}
