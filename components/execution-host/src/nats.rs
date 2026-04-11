use std::{
    collections::{HashMap, HashSet},
    path::{Path, PathBuf},
    time::Duration,
};

use anyhow::Result;
use futures_util::StreamExt as _;

// Reads NATS credentials from a directory of files produced by a Kubernetes
// secret volume mount.  Each key in the secret becomes a file whose contents
// are the value.  The four expected files are NATS_USERNAME, NATS_PASSWORD,
// NATS_HOST, and NATS_PORT.
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

// Manages the NATS client lifecycle: reads credentials from disk, connects,
// and broadcasts the live client via `client_tx`.  On any auth violation or
// lost connection the manager clears the broadcast (signalling downstream that
// NATS is unavailable), re-reads credentials from disk, and reconnects.
//
// `ready_tx` is set to `true` once an initial connection is established and
// back to `false` while reconnecting.  `credentials_path` is re-read on every
// connection attempt so that rotated credentials are picked up automatically.
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

        // Channel used by the event handler to signal an auth violation back
        // to this loop so we can break out of the wait and retry immediately.
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

                // Wait until an auth violation signals us to reconnect.
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

// Watches both the live NATS client and the desired topic set.  Whenever
// either changes, subscriptions are reconciled: new topics are subscribed,
// removed topics are cancelled, and a client replacement causes a full
// re-subscribe on the new connection.  When the client watch yields `None`
// (NATS is reconnecting) all subscriptions are dropped.
//
// All received messages are forwarded to `msg_tx`.
pub async fn manage_nats_subscriptions(
    mut client_rx: tokio::sync::watch::Receiver<Option<async_nats::Client>>,
    mut topics_rx: tokio::sync::watch::Receiver<Vec<String>>,
    msg_tx: tokio::sync::mpsc::Sender<async_nats::Message>,
) {
    // Keys are topic strings; values are oneshot senders used to cancel the
    // per-topic forwarding task (dropping the sender signals the task to stop,
    // which drops the Subscriber and sends UNSUB to the server).
    let mut subscriptions: HashMap<String, tokio::sync::oneshot::Sender<()>> = HashMap::new();

    loop {
        let client_changed = tokio::select! {
            result = client_rx.changed() => {
                if result.is_err() { break; }
                true
            }
            result = topics_rx.changed() => {
                if result.is_err() { break; }
                false
            }
        };

        let client = client_rx.borrow().clone();
        let desired: HashSet<String> = topics_rx.borrow_and_update().iter().cloned().collect();

        // Drop all existing subscriptions whenever the client changes (new
        // connection, or connection cleared) or when there is no live client.
        if client_changed || client.is_none() {
            subscriptions.clear();
        }

        let Some(client) = client else { continue };

        let current: HashSet<String> = subscriptions.keys().cloned().collect();

        for topic in current.difference(&desired) {
            subscriptions.remove(topic);
            tracing::info!(%topic, "unsubscribed from NATS topic");
        }

        for topic in desired.difference(&current) {
            // Use queue_subscribe so that when multiple execution-host
            // replicas are running (or during a rolling update) each message
            // is delivered to exactly one replica rather than all of them.
            // The queue group name is the topic itself — one competing group
            // per subject, mirroring the Kafka consumer-group pattern.
            match client.queue_subscribe(topic.clone(), topic.clone()).await {
                Ok(sub) => {
                    let (cancel_tx, cancel_rx) = tokio::sync::oneshot::channel::<()>();
                    let tx = msg_tx.clone();
                    let t = topic.clone();
                    tokio::spawn(async move {
                        let mut sub = sub;
                        tokio::select! {
                            _ = cancel_rx => {}
                            _ = async move {
                                while let Some(msg) = sub.next().await {
                                    if tx.send(msg).await.is_err() {
                                        break;
                                    }
                                }
                            } => {}
                        }
                    });
                    subscriptions.insert(topic.clone(), cancel_tx);
                    tracing::info!(%t, "subscribed to NATS topic");
                }
                Err(err) => {
                    tracing::error!(%topic, "failed to subscribe to NATS topic: {err:#}");
                }
            }
        }
    }
}
