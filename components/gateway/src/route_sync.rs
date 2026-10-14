use std::time::Duration;

use anyhow::Result;
use platform_common::identity::{
    ProjectedTokenReader, RPC_TIMEOUT, SYNC_SESSION_TIMEOUT, operator_channel,
};

use crate::route_table::{RouteEntry, RouteTable};

pub mod gateway {
    tonic::include_proto!("gateway.v1");
}

use gateway::{FullRoutesRequest, RouteUpdateAck, gateway_routes_client::GatewayRoutesClient};

/// Loops forever: fetches a full route snapshot then maintains the incremental
/// update stream.  On any error or clean stream close, backs off and retries.
/// `synced_tx` is true only after an authoritative snapshot and authenticated
/// stream registration; reconnecting revokes admission.
pub async fn run_route_sync_loop(
    addr: String,
    gateway_id: String,
    token: ProjectedTokenReader,
    table: RouteTable,
    synced_tx: tokio::sync::watch::Sender<bool>,
) {
    let mut backoff = Duration::from_secs(1);
    loop {
        let result = tokio::time::timeout(
            SYNC_SESSION_TIMEOUT,
            run_route_sync(&addr, &gateway_id, &token, &table, &synced_tx),
        )
        .await
        .unwrap_or_else(|_| Err(anyhow::anyhow!("route sync session expired")));
        if *synced_tx.borrow() {
            backoff = Duration::from_secs(1);
        }
        let _ = synced_tx.send(false);
        match result {
            Ok(()) => {
                tracing::warn!("route sync stream closed; reconnecting");
                backoff = Duration::from_secs(1);
            }
            Err(err) => {
                tracing::warn!(
                    error = %err,
                    retry_after_secs = backoff.as_secs(),
                    "route sync failed; reconnecting"
                );
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
    token: &ProjectedTokenReader,
    table: &RouteTable,
    synced_tx: &tokio::sync::watch::Sender<bool>,
) -> Result<()> {
    fetch_full_routes(addr.to_string(), gateway_id.to_string(), token, table).await?;

    tracing::info!("opening incremental route update stream");
    let mut client = operator_channel(addr)
        .await
        .map(GatewayRoutesClient::new)
        .map_err(|_| anyhow::anyhow!("route update connection failed"))?;
    tracing::info!("gRPC client connected for route updates");

    // Bridge tokio mpsc → futures Stream so tonic can consume acks.
    let (ack_tx, ack_rx) = tokio::sync::mpsc::channel::<RouteUpdateAck>(16);
    let ack_stream = futures_util::stream::unfold(ack_rx, |mut rx| async move {
        tracing::debug!("ack_stream: waiting for next ack from mpsc");
        let item = rx.recv().await;
        tracing::debug!(has_item = item.is_some(), "ack_stream: polled");
        item.map(|ack| (ack, rx))
    });

    tracing::info!("calling push_route_update RPC");
    let mut update_stream = client
        .push_route_update(token.grpc_request(ack_stream, SYNC_SESSION_TIMEOUT).await?)
        .await
        .map_err(|error| anyhow::anyhow!("route update RPC failed ({:?})", error.code()))?
        .into_inner();
    tracing::info!("push_route_update stream opened");

    // Send an initial ack immediately so the server can identify and register
    // this gateway before any updates are broadcast. Without this the server
    // blocks on Recv() waiting for an id while we block on message() waiting
    // for an update — a deadlock that prevents incremental route delivery.
    let initial_ack = RouteUpdateAck {
        gateway_id: gateway_id.to_string(),
        version_applied: String::new(),
        success: true,
        message: String::new(),
    };
    tracing::info!("sending initial gateway ack");
    if ack_tx.send(initial_ack).await.is_err() {
        tracing::warn!("failed to send initial gateway ack — channel closed");
        return Ok(());
    }
    tracing::info!("initial gateway ack sent to mpsc channel");
    let _ = synced_tx.send(true);

    tracing::info!("waiting for first route update message");
    while let Some(request) = update_stream
        .message()
        .await
        .map_err(|error| anyhow::anyhow!("route update stream failed ({:?})", error.code()))?
    {
        if let Some(update_config) = request.update {
            let version = update_config.version.clone();
            let update_count = update_config.updates.len();

            for update in update_config.updates {
                if let Some(route) = update.route {
                    if update.delete {
                        table.remove(&route.path)?;
                        tracing::info!(path = %route.path, "route removed");
                    } else {
                        table.upsert(
                            route.path.clone(),
                            RouteEntry {
                                methods: route.methods,
                                nats_subject: route.nats_subject,
                            },
                        )?;
                        tracing::info!(path = %route.path, "route upserted");
                    }
                }
            }

            tracing::info!(version, update_count, "route update applied");

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
async fn fetch_full_routes(
    addr: String,
    gateway_id: String,
    token: &ProjectedTokenReader,
    table: &RouteTable,
) -> Result<()> {
    tracing::info!("connecting to operator for full routes");
    let mut client = operator_channel(&addr)
        .await
        .map(GatewayRoutesClient::new)
        .map_err(|_| anyhow::anyhow!("route snapshot connection failed"))?;
    let response = client
        .request_full_routes(
            token
                .grpc_request(
                    FullRoutesRequest {
                        gateway_id: gateway_id.clone(),
                    },
                    RPC_TIMEOUT,
                )
                .await?,
        )
        .await
        .map_err(|error| anyhow::anyhow!("route snapshot RPC failed ({:?})", error.code()))?
        .into_inner();

    if !response.success {
        return Err(anyhow::anyhow!("operator rejected full routes request"));
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

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test]
    async fn invalid_operator_address_errors_exclude_credentials_and_tokens() {
        let table = RouteTable::new();
        let error = tokio::time::timeout(
            Duration::from_secs(3),
            fetch_full_routes(
                "http://user:OPERATOR_PASSWORD@[invalid?token=OPERATOR_TOKEN".to_string(),
                "gateway".to_string(),
                &ProjectedTokenReader::platform(),
                &table,
            ),
        )
        .await
        .expect("route snapshot exceeded test deadline")
        .unwrap_err();
        let diagnostic = format!("{error:#}\n{error:?}");
        assert_eq!(error.chain().count(), 1, "{diagnostic}");
        assert!(!diagnostic.contains("OPERATOR_PASSWORD"), "{diagnostic}");
        assert!(!diagnostic.contains("OPERATOR_TOKEN"), "{diagnostic}");
        assert_eq!(error.to_string(), "route snapshot connection failed");
    }
}
