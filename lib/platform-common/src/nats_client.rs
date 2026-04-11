use std::{
    path::{Path, PathBuf},
    time::Duration,
};

use anyhow::Result;

fn read_credentials(dir: &Path) -> Result<(String, async_nats::ConnectOptions)> {
    let read = |name: &str| -> Result<String> {
        let path = dir.join(name);
        std::fs::read_to_string(&path)
            .map(|s| s.trim().to_string())
            .map_err(|e| anyhow::anyhow!("failed to read {}: {e}", path.display()))
    };

    let username = read("NATS_USERNAME")?;
    let password = read("NATS_PASSWORD")?;
    let host = read("NATS_HOST")?;
    let port = read("NATS_PORT")?;

    let url = format!("nats://{}:{}", host, port);
    let opts = async_nats::ConnectOptions::new().user_and_password(username, password);
    Ok((url, opts))
}

/// Manages the NATS client lifecycle with credential rotation and automatic
/// reconnection on auth violations.
pub async fn run_nats_manager(
    credentials_path: PathBuf,
    client_tx: tokio::sync::watch::Sender<Option<async_nats::Client>>,
    ready_tx: tokio::sync::watch::Sender<bool>,
) {
    let mut backoff = Duration::from_secs(1);
    loop {
        let (url, opts) = match read_credentials(&credentials_path) {
            Ok(pair) => pair,
            Err(err) => {
                tracing::warn!("failed to read NATS credentials: {err:#}; retrying in {backoff:?}");
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(Duration::from_secs(30));
                continue;
            }
        };

        let (auth_err_tx, mut auth_err_rx) = tokio::sync::mpsc::channel::<()>(1);

        let opts = opts.event_callback(move |event| {
            let tx = auth_err_tx.clone();
            async move {
                match event {
                    async_nats::Event::ServerError(async_nats::ServerError::AuthorizationViolation) => {
                        tracing::warn!("NATS authorization violation; will re-read credentials and reconnect");
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

                auth_err_rx.recv().await;

                tracing::warn!("NATS client invalidated; clearing and reconnecting");
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
