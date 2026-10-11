use std::{collections::HashSet, pin::Pin, task::Poll, time::Duration};

use futures_util::{Stream, StreamExt as _, stream::SelectAll};

use crate::dispatch_metrics::{DispatchMetrics, QueuedWork};

pub(crate) struct InvocationMessage {
    pub(crate) message: async_nats::Message,
    pub(crate) invocation: crate::invocation::Invocation,
    pub(crate) queued: QueuedWork,
}

pub use platform_common::nats_client::run_nats_manager;

struct TopicSubscription {
    topic: String,
    subscriber: async_nats::Subscriber,
}

impl Stream for TopicSubscription {
    type Item = async_nats::Message;

    fn poll_next(
        mut self: Pin<&mut Self>,
        cx: &mut std::task::Context<'_>,
    ) -> Poll<Option<Self::Item>> {
        Pin::new(&mut self.subscriber).poll_next(cx)
    }
}

/// Reconciles queue subscriptions with client and topic updates, forwarding
/// received messages only after reserving space in the dispatch queue.
pub(crate) async fn manage_nats_subscriptions(
    mut client_rx: tokio::sync::watch::Receiver<Option<async_nats::Client>>,
    mut topics_rx: tokio::sync::watch::Receiver<Vec<String>>,
    msg_tx: tokio::sync::mpsc::Sender<InvocationMessage>,
    mut shutdown_rx: tokio::sync::broadcast::Receiver<()>,
    timeout: Duration,
    metrics: DispatchMetrics,
) {
    let mut subscriptions = SelectAll::<TopicSubscription>::new();
    let mut reconcile = true;

    'manage: loop {
        if reconcile {
            reconcile = false;
            let (client, client_changed) = {
                let client = client_rx.borrow_and_update();
                (client.clone(), client.has_changed())
            };
            if client_changed {
                subscriptions.clear();
            }
            let desired: HashSet<String> = topics_rx.borrow_and_update().iter().cloned().collect();

            subscriptions = std::mem::take(&mut subscriptions)
                .into_iter()
                .filter(|subscription| {
                    let keep = client.is_some() && desired.contains(&subscription.topic);
                    if !keep {
                        tracing::info!(topic = %subscription.topic, "unsubscribed from NATS topic");
                    }
                    keep
                })
                .collect();

            if let Some(client) = client {
                let current: HashSet<String> = subscriptions
                    .iter()
                    .map(|subscription| subscription.topic.clone())
                    .collect();

                for topic in desired.difference(&current) {
                    let result = tokio::select! {
                        biased;
                        _ = shutdown_rx.recv() => break 'manage,
                        _ = msg_tx.closed() => break 'manage,
                        result = client_rx.changed() => {
                            if result.is_err() { break 'manage; }
                            subscriptions.clear();
                            reconcile = true;
                            continue 'manage;
                        }
                        result = topics_rx.changed() => {
                            if result.is_err() { break 'manage; }
                            reconcile = true;
                            continue 'manage;
                        }
                        result = tokio::time::timeout(
                            Duration::from_secs(2),
                            client.queue_subscribe(topic.clone(), topic.clone()),
                        ) => result,
                    };

                    match result {
                        Ok(Ok(subscriber)) => {
                            subscriptions.push(TopicSubscription {
                                topic: topic.clone(),
                                subscriber,
                            });
                            tracing::info!(%topic, "subscribed to NATS topic");
                        }
                        Ok(Err(_)) => {
                            tracing::error!(%topic, "failed to subscribe to NATS topic");
                        }
                        Err(_) => {
                            tracing::error!(%topic, "NATS topic subscription timed out");
                        }
                    }
                }
            }
        }

        tokio::select! {
            biased;
            _ = shutdown_rx.recv() => break,
            _ = msg_tx.closed() => break,
            result = client_rx.changed() => {
                if result.is_err() { break; }
                subscriptions.clear();
                reconcile = true;
            }
            result = topics_rx.changed() => {
                if result.is_err() { break; }
                reconcile = true;
            }
            received = async {
                // Reserving first prevents one retained payload per topic when
                // dispatch is backpressured.
                let permit = msg_tx.clone().reserve_owned().await.ok()?;
                let message = subscriptions.next().await?;
                Some((permit, message))
            }, if !subscriptions.is_empty() => {
                if let Some((permit, message)) = received {
                    let invocation = crate::invocation::Invocation::with_metrics(timeout, metrics.clone());
                    permit.send(InvocationMessage { message, invocation, queued: metrics.queued() });
                }
            }
        }
    }

    tracing::info!("NATS subscription manager stopped; clearing subscriptions");
}
