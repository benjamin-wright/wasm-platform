use std::time::Duration;

use anyhow::Result;

/// Connection information for a NATS server, pushed to the execution-host via
/// the configsync gRPC stream instead of being read from a mounted Secret.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct NatsConnectionInfo {
    pub url: String,
    pub username: String,
    pub password: String,
}

/// Manages the NATS client lifecycle with credential rotation and automatic
/// reconnection on auth violations.
///
/// Unlike the previous file-backed implementation, `conn_rx` is a watch channel
/// carrying `Option<NatsConnectionInfo>`:
/// - `None`  → NATS is not yet provisioned; clear any live client and wait.
/// - `Some`  → connect (or reconnect) using the supplied credentials.
///
/// The manager reconnects automatically whenever the connection info changes
/// (credential rotation) or when an `AuthorizationViolation` is received.
pub async fn run_nats_manager(
    mut conn_rx: tokio::sync::watch::Receiver<Option<NatsConnectionInfo>>,
    client_tx: tokio::sync::watch::Sender<Option<async_nats::Client>>,
    ready_tx: tokio::sync::watch::Sender<bool>,
) {
    let mut backoff = Duration::from_secs(1);

    loop {
        // Wait until we have connection info.
        let info = loop {
            if let Some(info) = conn_rx.borrow_and_update().clone() {
                break info;
            }
            // No connection info yet — ensure client is cleared.
            let _ = ready_tx.send(false);
            let _ = client_tx.send(None);
            backoff = Duration::from_secs(1);
            if conn_rx.changed().await.is_err() {
                return;
            }
        };

        let url = info.url.clone();
        let opts = async_nats::ConnectOptions::new()
            .user_and_password(info.username.clone(), info.password.clone());

        let (auth_err_tx, mut auth_err_rx) = tokio::sync::mpsc::channel::<()>(1);

        let opts = opts.event_callback(move |event| {
            let tx = auth_err_tx.clone();
            async move {
                match event {
                    async_nats::Event::ServerError(
                        async_nats::ServerError::AuthorizationViolation,
                    ) => {
                        tracing::warn!(
                            "NATS authorization violation; will re-read credentials and reconnect"
                        );
                        let _ = tx.try_send(());
                    }
                    async_nats::Event::Disconnected => {
                        tracing::warn!("NATS disconnected");
                    }
                    async_nats::Event::Connected => {
                        tracing::info!("NATS reconnected");
                    }
                    _ => {}
                }
            }
        });

        match opts.connect(&url).await {
            Ok(client) => {
                tracing::info!(%url, "connected to NATS");
                backoff = Duration::from_secs(1);
                let _ = ready_tx.send(true);
                let _ = client_tx.send(Some(client));

                // Wait for either an auth error or new connection info (credential rotation).
                tokio::select! {
                    _ = auth_err_rx.recv() => {
                        tracing::warn!("NATS client invalidated by auth error; reconnecting");
                    }
                    result = conn_rx.changed() => {
                        if result.is_err() {
                            return;
                        }
                        tracing::info!("NATS connection info changed; reconnecting");
                    }
                }

                let _ = ready_tx.send(false);
                let _ = client_tx.send(None);
            }
            Err(err) => {
                tracing::warn!("failed to connect to NATS: {err:#}; retrying in {backoff:?}");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        }
    }
}
