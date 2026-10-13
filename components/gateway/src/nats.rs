use std::{
    collections::HashMap,
    sync::{Arc, Mutex},
    time::Duration,
};

use futures_util::{StreamExt, future::BoxFuture};
use platform_common::nats_client::{NatsConnectionInfo, run_nats_manager_bounded};
use tokio::{
    sync::{oneshot, watch},
    time::Instant,
};

type Reply = Result<Vec<u8>, RequestError>;

#[derive(Debug)]
pub enum RequestError {
    Unavailable,
    PayloadTooLarge,
    Failed,
    Timeout,
}

pub trait RequestBroker: Send + Sync {
    fn is_available(&self) -> bool;

    fn request(&self, subject: String, payload: Vec<u8>, deadline: Instant)
    -> BoxFuture<'_, Reply>;
}

pub struct NatsBroker {
    session: watch::Receiver<Option<Arc<ReplySession>>>,
}

struct ReplySession {
    client: async_nats::Client,
    prefix: String,
    pending: Arc<PendingReplies>,
}

impl NatsBroker {
    pub fn new(info: NatsConnectionInfo, ready_tx: watch::Sender<bool>, capacity: usize) -> Self {
        let (client_tx, client_rx) = watch::channel(None);
        let (connection_tx, connection_rx) = watch::channel(Some(info));
        let (connected_tx, _) = watch::channel(false);
        tokio::spawn(async move {
            let _connection_tx = connection_tx;
            run_nats_manager_bounded(connection_rx, client_tx, connected_tx, capacity).await;
        });
        let (session_tx, session) = watch::channel(None);
        tokio::spawn(run_reply_sessions(
            client_rx, session_tx, ready_tx, capacity,
        ));
        Self { session }
    }
}

impl RequestBroker for NatsBroker {
    fn is_available(&self) -> bool {
        self.session.borrow().as_ref().is_some_and(|session| {
            session.client.connection_state() == async_nats::connection::State::Connected
        })
    }

    fn request(
        &self,
        subject: String,
        payload: Vec<u8>,
        deadline: Instant,
    ) -> BoxFuture<'_, Reply> {
        let session = self.session.borrow().clone();
        Box::pin(async move {
            if Instant::now() >= deadline {
                return Err(RequestError::Timeout);
            }
            let session = session.ok_or(RequestError::Unavailable)?;
            if session.client.connection_state() != async_nats::connection::State::Connected {
                return Err(RequestError::Unavailable);
            }
            if payload.len() > session.client.server_info().max_payload {
                return Err(RequestError::PayloadTooLarge);
            }
            let (pending, receiver) = session.pending.reserve()?;
            let reply_subject = format!("{}{}", session.prefix, pending.id);
            let request = async {
                session
                    .client
                    .publish_with_reply(subject, reply_subject, payload.into())
                    .await
                    .map_err(|error| {
                        tracing::warn!(error_kind = ?error.kind(), "NATS publish failed");
                        RequestError::Failed
                    })?;
                receiver.await.map_err(|_| RequestError::Unavailable)?
            };
            let result = tokio::time::timeout_at(deadline, request)
                .await
                .map_err(|_| RequestError::Timeout)?;
            drop(pending);
            result
        })
    }
}

struct PendingState {
    next_id: u64,
    retired: bool,
    replies: HashMap<u64, oneshot::Sender<Reply>>,
}

struct PendingReplies {
    capacity: usize,
    state: Mutex<PendingState>,
}

impl PendingReplies {
    fn new(capacity: usize) -> Self {
        Self {
            capacity,
            state: Mutex::new(PendingState {
                next_id: 0,
                retired: false,
                replies: HashMap::new(),
            }),
        }
    }

    fn reserve(self: &Arc<Self>) -> Result<(PendingReply, oneshot::Receiver<Reply>), RequestError> {
        let (sender, receiver) = oneshot::channel();
        let id = {
            let mut state = self.state.lock().map_err(|_| RequestError::Failed)?;
            if state.retired || state.replies.len() >= self.capacity {
                return Err(RequestError::Unavailable);
            }
            let id = state.next_id;
            state.next_id = id.checked_add(1).ok_or(RequestError::Failed)?;
            state.replies.insert(id, sender);
            id
        };
        Ok((
            PendingReply {
                owner: Arc::clone(self),
                id,
            },
            receiver,
        ))
    }

    fn remove(&self, id: u64) -> Result<Option<oneshot::Sender<Reply>>, RequestError> {
        Ok(self
            .state
            .lock()
            .map_err(|_| RequestError::Failed)?
            .replies
            .remove(&id))
    }

    fn deliver(&self, id: u64, reply: Reply) -> Result<bool, RequestError> {
        Ok(self
            .remove(id)?
            .is_some_and(|sender| sender.send(reply).is_ok()))
    }

    fn retire(&self) -> Result<(), RequestError> {
        let replies = {
            let mut state = self.state.lock().map_err(|_| RequestError::Failed)?;
            state.retired = true;
            std::mem::take(&mut state.replies)
        };
        drop(replies);
        Ok(())
    }
}

struct PendingReply {
    owner: Arc<PendingReplies>,
    id: u64,
}

impl Drop for PendingReply {
    fn drop(&mut self) {
        let _ = self.owner.remove(self.id);
    }
}

async fn run_reply_sessions(
    mut clients: watch::Receiver<Option<async_nats::Client>>,
    sessions: watch::Sender<Option<Arc<ReplySession>>>,
    ready: watch::Sender<bool>,
    capacity: usize,
) {
    loop {
        sessions.send_replace(None);
        ready.send_replace(false);
        let client = clients.borrow_and_update().clone();
        let Some(client) = client else {
            if clients.changed().await.is_err() {
                return;
            }
            continue;
        };
        let prefix = format!("{}.", client.new_inbox());
        let subscription = tokio::time::timeout(
            Duration::from_secs(5),
            client.subscribe(format!("{prefix}*")),
        )
        .await;
        let Ok(Ok(mut subscription)) = subscription else {
            tokio::select! {
                _ = tokio::time::sleep(Duration::from_secs(1)) => {},
                result = clients.changed() => {
                    if result.is_err() { return; }
                }
            }
            continue;
        };
        let session = Arc::new(ReplySession {
            client,
            prefix,
            pending: Arc::new(PendingReplies::new(capacity)),
        });
        sessions.send_replace(Some(Arc::clone(&session)));
        ready.send_replace(true);
        let clients_closed = loop {
            tokio::select! {
                result = clients.changed() => {
                    break result.is_err();
                }
                message = subscription.next() => {
                    let Some(message) = message else { break false; };
                    let Some(id) = message.subject.strip_prefix(&session.prefix)
                        .and_then(|id| id.parse::<u64>().ok()) else { continue; };
                    let reply = if message.status.is_some() {
                        Err(RequestError::Failed)
                    } else {
                        Ok(message.payload.to_vec())
                    };
                    if let Err(error) = session.pending.deliver(id, reply) {
                        tracing::warn!(?error, "NATS pending reply delivery failed");
                        break false;
                    }
                }
            }
        };
        if let Err(error) = session.pending.retire() {
            tracing::warn!(?error, "NATS pending reply retirement failed");
        }
        if clients_closed {
            sessions.send_replace(None);
            ready.send_replace(false);
            return;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::sync::oneshot::error::TryRecvError;

    #[test]
    fn dropping_registration_closes_waiter_and_releases_capacity() {
        let replies = Arc::new(PendingReplies::new(1));
        let (pending, mut receiver) = replies.reserve().unwrap();
        assert!(matches!(receiver.try_recv(), Err(TryRecvError::Empty)));

        drop(pending);

        assert!(matches!(receiver.try_recv(), Err(TryRecvError::Closed)));
        assert!(replies.reserve().is_ok());
    }

    #[test]
    fn reply_delivery_removes_registration_exactly_once() {
        let replies = Arc::new(PendingReplies::new(1));
        let (pending, mut receiver) = replies.reserve().unwrap();

        assert!(replies.deliver(pending.id, Ok(vec![1, 2])).unwrap());
        assert!(!replies.deliver(pending.id, Ok(vec![3])).unwrap());
        assert_eq!(receiver.try_recv().unwrap().unwrap(), vec![1, 2]);
        assert!(replies.reserve().is_ok());
        drop(pending);
    }

    #[test]
    fn retirement_closes_waiters_and_rejects_stale_reservations() {
        let replies = Arc::new(PendingReplies::new(2));
        let (first, mut first_receiver) = replies.reserve().unwrap();
        let (second, mut second_receiver) = replies.reserve().unwrap();

        replies.retire().unwrap();

        assert!(matches!(
            first_receiver.try_recv(),
            Err(TryRecvError::Closed)
        ));
        assert!(matches!(
            second_receiver.try_recv(),
            Err(TryRecvError::Closed)
        ));
        assert!(matches!(replies.reserve(), Err(RequestError::Unavailable)));
        assert!(!replies.deliver(first.id, Ok(vec![1])).unwrap());
        drop((first, second));
        assert!(matches!(replies.reserve(), Err(RequestError::Unavailable)));
    }

    #[test]
    fn capacity_recovers_without_delivering_late_replies_to_new_waiters() {
        let replies = Arc::new(PendingReplies::new(1));
        let (first, mut first_receiver) = replies.reserve().unwrap();
        let first_id = first.id;
        assert!(matches!(replies.reserve(), Err(RequestError::Unavailable)));

        drop(first);
        assert!(matches!(
            first_receiver.try_recv(),
            Err(TryRecvError::Closed)
        ));
        let (second, mut second_receiver) = replies.reserve().unwrap();
        assert!(!replies.deliver(first_id, Ok(vec![1])).unwrap());
        assert!(matches!(
            second_receiver.try_recv(),
            Err(TryRecvError::Empty)
        ));
        assert!(replies.deliver(second.id, Ok(vec![2])).unwrap());
        assert_eq!(second_receiver.try_recv().unwrap().unwrap(), vec![2]);
    }
}
