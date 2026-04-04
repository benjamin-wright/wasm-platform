use std::collections::{HashMap, HashSet};

use anyhow::Result;
use futures_util::StreamExt as _;

// Connects to the NATS server using credentials from environment variables
// injected by the db-operator-managed secret.
//
// All four variables (NATS_USERNAME, NATS_PASSWORD, NATS_HOST, NATS_PORT) must
// be set together.  If some but not all are present, the configuration is
// ambiguous and an error is returned rather than falling back silently to
// unauthenticated localhost.  When none are set, the function falls back to
// unauthenticated localhost for local development.
pub async fn connect() -> Result<async_nats::Client> {
    let username = std::env::var("NATS_USERNAME").ok();
    let password = std::env::var("NATS_PASSWORD").ok();
    let host = std::env::var("NATS_HOST").ok();
    let port = std::env::var("NATS_PORT").ok();

    let present = [&username, &password, &host, &port]
        .iter()
        .filter(|v| v.is_some())
        .count();

    let (url, opts) = match present {
        4 => {
            let url = format!("nats://{}:{}", host.unwrap(), port.unwrap());
            let opts = async_nats::ConnectOptions::new()
                .user_and_password(username.unwrap(), password.unwrap());
            (url, opts)
        }
        0 => (
            "nats://localhost:4222".to_string(),
            async_nats::ConnectOptions::new(),
        ),
        n => {
            return Err(anyhow::anyhow!(
                "partial NATS credentials: {n}/4 variables set; \
                 NATS_USERNAME, NATS_PASSWORD, NATS_HOST, and NATS_PORT must all be set together"
            ))
        }
    };

    Ok(opts.connect(&url).await?)
}

// Watches the topic set published by the config sync loop and maintains one
// NATS subscription per topic.  All messages are forwarded into msg_tx.
pub async fn manage_nats_subscriptions(
    client: async_nats::Client,
    mut topics_rx: tokio::sync::watch::Receiver<Vec<String>>,
    msg_tx: tokio::sync::mpsc::Sender<async_nats::Message>,
) {
    // Keys are topic strings; values are oneshot senders used to cancel the
    // per-topic forwarding task (dropping the sender signals the task to stop,
    // which drops the Subscriber and sends UNSUB to the server).
    let mut subscriptions: HashMap<String, tokio::sync::oneshot::Sender<()>> = HashMap::new();

    loop {
        if topics_rx.changed().await.is_err() {
            break;
        }
        let desired: HashSet<String> = topics_rx.borrow_and_update().iter().cloned().collect();
        let current: HashSet<String> = subscriptions.keys().cloned().collect();

        for topic in current.difference(&desired) {
            subscriptions.remove(topic);
            tracing::info!(%topic, "unsubscribed from NATS topic");
        }

        for topic in desired.difference(&current) {
            match client.subscribe(topic.clone()).await {
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
