use std::time::Duration;

use anyhow::Result;

use crate::route_table::{RouteEntry, RouteTable};

pub mod gateway {
    tonic::include_proto!("gateway.v1");
}

use gateway::{
    FullRoutesRequest, RouteUpdateAck, gateway_routes_client::GatewayRoutesClient,
};

/// Loops forever: fetches a full route snapshot then maintains the incremental
/// update stream.  On any error or clean stream close, backs off and retries.
/// `synced_tx` is set to `true` after the first successful full snapshot and
/// back to `false` while reconnecting.
pub async fn run_route_sync_loop(
    addr: String,
    gateway_id: String,
    table: RouteTable,
    synced_tx: tokio::sync::watch::Sender<bool>,
) {
    let mut backoff = Duration::from_secs(1);
    loop {
        match run_route_sync(&addr, &gateway_id, &table, &synced_tx).await {
            Ok(()) => {
                tracing::warn!("route sync stream closed; reconnecting");
                backoff = Duration::from_secs(1);
            }
            Err(err) => {
                tracing::warn!("route sync error: {err:#}; reconnecting in {backoff:?}");
                let _ = synced_tx.send(false);
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        }
        tokio::time::sleep(backoff).await;
    }
}

/// Fetches a full route snapshot then drives a single incremental update stream
/// session until it closes or errors.
async fn run_route_sync(
    addr: &str,
    gateway_id: &str,
    table: &RouteTable,
    synced_tx: &tokio::sync::watch::Sender<bool>,
) -> Result<()> {
    fetch_full_routes(addr.to_string(), gateway_id.to_string(), table).await?;
    let _ = synced_tx.send(true);

    tracing::info!("opening incremental route update stream");
    let mut client = GatewayRoutesClient::connect(addr.to_string()).await?;

    // Bridge tokio mpsc → futures Stream so tonic can consume acks.
    let (ack_tx, ack_rx) = tokio::sync::mpsc::channel::<RouteUpdateAck>(16);
    let ack_stream = futures_util::stream::unfold(ack_rx, |mut rx| async move {
        rx.recv().await.map(|ack| (ack, rx))
    });

    let mut update_stream = client
        .push_route_update(ack_stream)
        .await?
        .into_inner();

    while let Some(request) = update_stream.message().await? {
        if let Some(update_config) = request.update {
            let version = update_config.version.clone();
            let update_count = update_config.updates.len();

            for update in update_config.updates {
                if let Some(route) = update.route {
                    if update.delete {
                        table.remove(&route.path)?;
                        tracing::debug!(path = %route.path, "route removed");
                    } else {
                        table.upsert(
                            route.path.clone(),
                            RouteEntry {
                                methods: route.methods,
                                nats_subject: route.nats_subject,
                            },
                        )?;
                        tracing::debug!(path = %route.path, "route upserted");
                    }
                }
            }

            tracing::debug!(version, update_count, "route update applied");

            let ack = RouteUpdateAck {
                gateway_id: gateway_id.to_string(),
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

/// Connects to the operator's gRPC endpoint, requests a full route snapshot,
/// and applies it to the table.  Returns an error if the connection or RPC
/// fails so that the process retries rather than silently running unconfigured.
async fn fetch_full_routes(addr: String, gateway_id: String, table: &RouteTable) -> Result<()> {
    tracing::info!(%addr, "connecting to operator for full routes");
    let mut client = GatewayRoutesClient::connect(addr).await?;
    let response = client
        .request_full_routes(FullRoutesRequest {
            gateway_id: gateway_id.clone(),
        })
        .await?
        .into_inner();

    if !response.success {
        return Err(anyhow::anyhow!(
            "full routes request failed: {}",
            response.message
        ));
    }

    let routes: Vec<_> = response
        .routes
        .into_iter()
        .map(|r| {
            (
                r.path.clone(),
                RouteEntry {
                    methods: r.methods,
                    nats_subject: r.nats_subject,
                },
            )
        })
        .collect();

    let count = routes.len();
    table.replace_all(routes)?;
    tracing::info!(routes = count, version = %response.version, "full route snapshot applied");
    Ok(())
}
