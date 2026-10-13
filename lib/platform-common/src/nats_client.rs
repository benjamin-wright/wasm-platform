use std::time::Duration;

/// Connection information for a NATS server, pushed to the execution-host via
/// the configsync gRPC stream instead of being read from a mounted Secret.
#[derive(Clone, PartialEq, Eq)]
pub struct NatsConnectionInfo {
    pub url: String,
    pub username: String,
    pub password: String,
}

impl std::fmt::Debug for NatsConnectionInfo {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("NatsConnectionInfo")
            .field("url", &"[REDACTED]")
            .field("username", &"[REDACTED]")
            .field("password", &"[REDACTED]")
            .finish()
    }
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
    conn_rx: tokio::sync::watch::Receiver<Option<NatsConnectionInfo>>,
    client_tx: tokio::sync::watch::Sender<Option<async_nats::Client>>,
    ready_tx: tokio::sync::watch::Sender<bool>,
) {
    run_nats_manager_inner(conn_rx, client_tx, ready_tx, None).await;
}

/// Runs the connection manager with bounded outgoing-command and subscription queues.
pub async fn run_nats_manager_bounded(
    conn_rx: tokio::sync::watch::Receiver<Option<NatsConnectionInfo>>,
    client_tx: tokio::sync::watch::Sender<Option<async_nats::Client>>,
    ready_tx: tokio::sync::watch::Sender<bool>,
    message_capacity: usize,
) {
    run_nats_manager_inner(conn_rx, client_tx, ready_tx, Some(message_capacity)).await;
}

async fn run_nats_manager_inner(
    mut conn_rx: tokio::sync::watch::Receiver<Option<NatsConnectionInfo>>,
    client_tx: tokio::sync::watch::Sender<Option<async_nats::Client>>,
    ready_tx: tokio::sync::watch::Sender<bool>,
    message_capacity: Option<usize>,
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
        let mut opts = async_nats::ConnectOptions::new()
            .user_and_password(info.username.clone(), info.password.clone());
        if let Some(capacity) = message_capacity {
            opts = opts
                .client_capacity(capacity)
                .subscription_capacity(capacity);
        }

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
                tracing::info!("connected to NATS");
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
                tracing::warn!(error_kind = ?err.kind(), retry_after_secs = backoff.as_secs(), "failed to connect to NATS");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
            }
        }
    }
}
