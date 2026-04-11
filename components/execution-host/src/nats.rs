use std::collections::{HashMap, HashSet};

use futures_util::StreamExt as _;

pub use platform_common::nats_client::run_nats_manager;

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
