use std::{collections::HashMap, future::Future, sync::Arc};

use anyhow::Result;
use tokio::{
    sync::{OwnedSemaphorePermit, Semaphore, broadcast, mpsc, watch},
    task::{AbortHandle, Id, JoinError, JoinHandle, JoinSet},
};

use crate::{
    config::{AppRegistry, FunctionEntry, configsync::WorldType},
    dispatch_metrics::{DispatchMetrics, TaskRecord},
    invocation::{BlockingInvocation, Invocation},
    metrics::MetricsRegistry,
    modules::ModuleRegistry,
    nats::{InvocationMessage, ReplyOutcome, ReplyTarget},
    runtime::{InvocationContext, RuntimeState, invoke_on_message, invoke_on_request},
};
use platform_common::http_types::{HttpRequestPayload, HttpResponsePayload};

pub(crate) struct DispatchContext {
    pub(crate) state: Arc<RuntimeState>,
    pub(crate) app_registry: AppRegistry,
    pub(crate) module_registry: ModuleRegistry,
    pub(crate) metrics_registry: MetricsRegistry,
    pub(crate) client_rx: watch::Receiver<Option<async_nats::Client>>,
    pub(crate) synced_rx: watch::Receiver<bool>,
}

struct FunctionIdentity {
    app_name: String,
    app_namespace: String,
    function_name: String,
}

impl From<&FunctionEntry> for FunctionIdentity {
    fn from(function: &FunctionEntry) -> Self {
        Self {
            app_name: function.app_name.clone(),
            app_namespace: function.app_namespace.clone(),
            function_name: function.function_name.clone(),
        }
    }
}

struct InvocationJob {
    invocation: Invocation,
    identity: FunctionIdentity,
    reply: ReplyTarget,
    record: TaskRecord,
    authority_revoked: bool,
}

enum OwnedJob {
    Execution(Box<InvocationJob>),
    Reply {
        execution_outcome: &'static str,
        abort: AbortHandle,
    },
}

enum Completion {
    Execution {
        result: Result<Option<Vec<u8>>>,
        cancelled: bool,
        expired: bool,
    },
    Reply,
}

/// Owns intake, accepted executions and replies until their actual completion.
pub(crate) struct InvocationSupervisor {
    messages: mpsc::Receiver<InvocationMessage>,
    intake: Option<JoinHandle<()>>,
    slots: Arc<Semaphore>,
    tasks: JoinSet<Completion>,
    jobs: HashMap<Id, OwnedJob>,
    metrics: DispatchMetrics,
    max_tracked: usize,
    stopped: bool,
}

impl InvocationSupervisor {
    pub(crate) fn new(
        max_concurrent: usize,
        metrics: DispatchMetrics,
        messages: mpsc::Receiver<InvocationMessage>,
        intake: JoinHandle<()>,
    ) -> Self {
        metrics.configure(messages.max_capacity() + 1, max_concurrent, max_concurrent);
        Self {
            messages,
            intake: Some(intake),
            slots: Arc::new(Semaphore::new(max_concurrent)),
            tasks: JoinSet::new(),
            jobs: HashMap::new(),
            metrics,
            max_tracked: max_concurrent,
            stopped: false,
        }
    }

    pub(crate) async fn run(
        &mut self,
        mut context: DispatchContext,
        mut shutdown: broadcast::Receiver<()>,
    ) {
        'dispatch: loop {
            let message = loop {
                tokio::select! {
                    biased;
                    _ = shutdown.recv() => break 'dispatch,
                    result = context.synced_rx.changed() => {
                        self.cancel_untrusted_work();
                        if result.is_err() { break 'dispatch; }
                    }
                    Some(result) = self.tasks.join_next_with_id(), if !self.tasks.is_empty() => {
                        self.collect(result);
                    }
                    message = self.messages.recv(), if self.jobs.len() < self.max_tracked => {
                        match message {
                            Some(message) => break message,
                            None => break 'dispatch,
                        }
                    }
                }
            };
            let InvocationMessage {
                message,
                invocation,
                mut queued,
            } = message;
            if !*context.synced_rx.borrow() {
                invocation.cancel();
                queued.set_outcome("error");
                continue;
            }
            let permit = {
                let admission = invocation.acquire_permit(self.slots.clone());
                tokio::pin!(admission);
                loop {
                    tokio::select! {
                        biased;
                        _ = shutdown.recv() => {
                            invocation.cancel();
                            break 'dispatch;
                        }
                        result = context.synced_rx.changed() => {
                            invocation.cancel();
                            queued.set_outcome("error");
                            self.cancel_untrusted_work();
                            if result.is_err() { break 'dispatch; }
                            continue 'dispatch;
                        }
                        Some(result) = self.tasks.join_next_with_id(), if !self.tasks.is_empty() => {
                            self.collect(result);
                        }
                        permit = &mut admission => break permit,
                    }
                }
            };
            let client = context.client_rx.borrow().clone();
            let reply = ReplyTarget::new(message.reply.clone(), client.clone());
            let permit = match permit {
                Ok(permit) => permit,
                Err(_) => {
                    queued.set_outcome("expired");
                    drop(queued);
                    let record = self.metrics.task_started();
                    self.spawn_reply(
                        record,
                        "timeout",
                        "ok",
                        reply.publish(error_response(504, "invocation timed out")),
                    );
                    continue;
                }
            };
            let subject = message.subject.as_str();
            let function = match context.app_registry.get_by_topic(subject) {
                Ok(Some(function)) => function,
                Ok(None) => {
                    queued.set_outcome("unknown_route");
                    tracing::warn!(%subject, "received message for unknown topic; dropping");
                    continue;
                }
                Err(_) => {
                    queued.set_outcome("error");
                    tracing::error!(%subject, "app registry lookup failed");
                    continue;
                }
            };
            let component = match context.module_registry.get(
                &function.app_namespace,
                &function.app_name,
                &function.function_name,
            ) {
                Ok(Some(component)) => component,
                Ok(None) => {
                    queued.set_outcome("module_unavailable");
                    tracing::warn!(namespace = %function.app_namespace, app_name = %function.app_name, function_name = %function.function_name, "module not yet loaded; dropping message");
                    continue;
                }
                Err(_) => {
                    queued.set_outcome("error");
                    tracing::error!("module registry lookup failed");
                    continue;
                }
            };
            if !*context.synced_rx.borrow() || context.synced_rx.has_changed().unwrap_or(true) {
                invocation.cancel();
                queued.set_outcome("error");
                continue;
            }
            queued.set_outcome("admitted");
            drop(queued);
            let trigger = match function.world_type {
                WorldType::Http => "http",
                WorldType::Message => "topic",
            };
            context.metrics_registry.record_event(
                &function.app_name,
                &function.app_namespace,
                trigger,
            );
            let job = InvocationJob {
                invocation,
                identity: FunctionIdentity::from(&function),
                reply,
                record: self.metrics.task_started(),
                authority_revoked: false,
            };
            let state = context.state.clone();
            let payload = message.payload.to_vec();
            self.spawn_execution(job, permit, move |invocation| {
                tokio::runtime::Handle::current().block_on(async move {
                    invocation.check().map_err(anyhow::Error::msg)?;
                    let world = function.world_type;
                    let context = InvocationContext {
                        function,
                        nats_client: client,
                        invocation,
                    };
                    match world {
                        WorldType::Message => {
                            invoke_on_message(&state, &component, &payload, context).await
                        }
                        WorldType::Http => {
                            let request: HttpRequestPayload = serde_json::from_slice(&payload)?;
                            let response =
                                invoke_on_request(&state, &component, request, context).await?;
                            Ok(Some(serde_json::to_vec(&response)?))
                        }
                    }
                })
            });
        }
        self.stop_and_drain().await;
    }

    fn spawn_execution(
        &mut self,
        job: InvocationJob,
        permit: OwnedSemaphorePermit,
        work: impl FnOnce(Invocation) -> Result<Option<Vec<u8>>> + Send + 'static,
    ) {
        let execution = BlockingInvocation::new(job.invocation.clone(), permit);
        let invocation = job.invocation.clone();
        let task = self.tasks.spawn_blocking(move || {
            let result = execution.run(work);
            Completion::Execution {
                result,
                cancelled: invocation.check().is_err(),
                expired: tokio::time::Instant::now() >= invocation.deadline(),
            }
        });
        self.jobs
            .insert(task.id(), OwnedJob::Execution(Box::new(job)));
    }

    fn spawn_reply(
        &mut self,
        mut record: TaskRecord,
        outcome: &'static str,
        execution_outcome: &'static str,
        reply: impl Future<Output = ReplyOutcome> + Send + 'static,
    ) {
        record.set_outcome("cancelled");
        let task = self.tasks.spawn(async move {
            let result = reply.await;
            record.set_outcome(outcome);
            if matches!(
                result,
                ReplyOutcome::Unavailable | ReplyOutcome::Failed | ReplyOutcome::TimedOut
            ) {
                tracing::error!(outcome = ?result, "invocation reply failed");
                if outcome == "success" {
                    record.set_outcome("error");
                }
            }
            drop(record);
            Completion::Reply
        });
        self.jobs.insert(
            task.id(),
            OwnedJob::Reply {
                execution_outcome,
                abort: task,
            },
        );
    }

    fn cancel_untrusted_work(&mut self) {
        for job in self.jobs.values_mut() {
            match job {
                OwnedJob::Execution(job) => {
                    job.authority_revoked = true;
                    job.invocation.cancel();
                }
                OwnedJob::Reply { abort, .. } => abort.abort(),
            }
        }
        while let Ok(mut message) = self.messages.try_recv() {
            message.invocation.cancel();
            message.queued.set_outcome("error");
        }
    }

    fn collect(&mut self, result: Result<(Id, Completion), JoinError>) {
        let id = match &result {
            Ok((id, _)) => *id,
            Err(error) => error.id(),
        };
        let Some(job) = self.jobs.remove(&id) else {
            tracing::error!("completed invocation has no owner");
            return;
        };
        match job {
            OwnedJob::Execution(mut job) => {
                if job.authority_revoked {
                    job.record.set_outcome("cancelled");
                    self.metrics.task_reaped("cancelled");
                    return;
                }
                let (cancelled, expired) = match &result {
                    Ok((
                        _,
                        Completion::Execution {
                            cancelled, expired, ..
                        },
                    )) => (*cancelled, *expired),
                    _ => (true, false),
                };
                let collection_outcome = join_outcome(&result);
                let (outcome, response) = match result {
                    Ok((_, Completion::Execution { .. })) if cancelled => (
                        if expired { "timeout" } else { "cancelled" },
                        error_response(
                            if expired { 504 } else { 500 },
                            if expired {
                                "invocation timed out"
                            } else {
                                "invocation cancelled"
                            },
                        ),
                    ),
                    Ok((
                        _,
                        Completion::Execution {
                            result: Ok(response),
                            ..
                        },
                    )) => ("success", response),
                    Ok((_, Completion::Execution { result: Err(_), .. })) => {
                        tracing::error!(app_name = %job.identity.app_name, app_namespace = %job.identity.app_namespace, "invocation failed");
                        (
                            "error",
                            error_response(500, "internal error: invocation failed"),
                        )
                    }
                    Err(error) => {
                        tracing::error!(
                            panicked = error.is_panic(),
                            cancelled = error.is_cancelled(),
                            "invocation task failed"
                        );
                        (
                            if error.is_panic() {
                                "panic"
                            } else {
                                "cancelled"
                            },
                            error_response(500, "internal error: execution panicked"),
                        )
                    }
                    Ok((_, Completion::Reply)) => {
                        unreachable!("execution job returned reply completion")
                    }
                };
                tracing::info!(app_name = %job.identity.app_name, app_namespace = %job.identity.app_namespace, function_name = %job.identity.function_name, cancelled, expired, "invocation stopped");
                self.spawn_reply(
                    job.record,
                    outcome,
                    collection_outcome,
                    job.reply.publish(response),
                );
            }
            OwnedJob::Reply {
                execution_outcome, ..
            } => {
                if let Err(error) = &result {
                    tracing::error!(
                        panicked = error.is_panic(),
                        cancelled = error.is_cancelled(),
                        "invocation reply task failed"
                    );
                }
                let outcome = if result.is_err() {
                    join_outcome(&result)
                } else {
                    execution_outcome
                };
                self.metrics.task_reaped(outcome);
            }
        }
    }

    async fn stop_and_drain(&mut self) {
        if !self.stopped {
            self.stopped = true;
            self.metrics.dispatch_stopped();
            self.messages.close();
        }
        if let Some(intake) = self.intake.as_mut() {
            let result = intake.await;
            self.intake.take();
            if let Err(error) = result {
                tracing::error!(
                    panicked = error.is_panic(),
                    cancelled = error.is_cancelled(),
                    "NATS intake task failed"
                );
            }
        }
        while let Ok(message) = self.messages.try_recv() {
            message.invocation.cancel();
        }
        tracing::info!("dispatch stopped; draining in-flight invocations");
        while let Some(result) = self.tasks.join_next_with_id().await {
            self.collect(result);
        }
        self.metrics.drain_finished();
        tracing::info!("drain complete; exiting");
    }
}

fn join_outcome<T>(result: &Result<T, JoinError>) -> &'static str {
    match result {
        Ok(_) => "ok",
        Err(error) if error.is_panic() => "panic",
        Err(_) => "cancelled",
    }
}

fn error_response(status: u16, message: &str) -> Option<Vec<u8>> {
    serde_json::to_vec(&HttpResponsePayload {
        status,
        headers: vec![("content-type".to_owned(), "text/plain".to_owned())],
        body: Some(message.as_bytes().to_vec()),
    })
    .ok()
}

#[cfg(test)]
mod tests {
    use std::{task::Poll, time::Duration};

    use prometheus::Registry;
    use tokio::sync::oneshot;

    use super::*;

    const TEST_TIMEOUT: Duration = Duration::from_secs(5);

    async fn bounded<F: Future>(future: F) -> F::Output {
        tokio::time::timeout(TEST_TIMEOUT, future)
            .await
            .expect("supervisor test exceeded its deadline")
    }

    fn supervisor() -> (
        InvocationSupervisor,
        mpsc::Sender<InvocationMessage>,
        Registry,
    ) {
        let registry = Registry::new();
        let metrics = DispatchMetrics::new(&registry).unwrap();
        metrics.configure(2, 1, 1);
        let (sender, messages) = mpsc::channel(1);
        let intake = tokio::spawn(async {});
        (
            InvocationSupervisor::new(1, metrics, messages, intake),
            sender,
            registry,
        )
    }

    fn job(supervisor: &InvocationSupervisor, invocation: Invocation) -> InvocationJob {
        InvocationJob {
            invocation,
            identity: FunctionIdentity {
                app_name: "test-app".to_owned(),
                app_namespace: "test-namespace".to_owned(),
                function_name: "test-function".to_owned(),
            },
            reply: ReplyTarget::new(None, None),
            record: supervisor.metrics.task_started(),
            authority_revoked: false,
        }
    }

    fn metric(registry: &Registry, name: &str, label: Option<(&str, &str)>) -> f64 {
        let family = registry
            .gather()
            .into_iter()
            .find(|family| family.name() == name)
            .expect("supervisor metric was not registered");
        family
            .get_metric()
            .iter()
            .filter(|metric| {
                label.is_none_or(|(name, value)| {
                    metric
                        .get_label()
                        .iter()
                        .any(|label| label.name() == name && label.value() == value)
                })
            })
            .map(|metric| metric.get_counter().value() + metric.get_gauge().value())
            .sum()
    }

    async fn cancel_pending_collection(supervisor: &mut InvocationSupervisor) {
        let mut collection = Box::pin(supervisor.tasks.join_next_with_id());
        std::future::poll_fn(|cx| {
            assert!(collection.as_mut().poll(cx).is_pending());
            Poll::Ready(())
        })
        .await;
        drop(collection);
    }

    async fn collect_next(supervisor: &mut InvocationSupervisor) {
        let result = bounded(supervisor.tasks.join_next_with_id())
            .await
            .expect("supervisor lost an owned task");
        supervisor.collect(result);
    }

    #[tokio::test]
    async fn uninitialized_authority_cancels_intake_before_registry_lookup() {
        let (mut supervisor, sender, registry) = supervisor();
        let invocation =
            Invocation::with_metrics(Duration::from_secs(30), supervisor.metrics.clone());
        sender
            .try_send(InvocationMessage {
                message: async_nats::Message {
                    subject: "fn.test".into(),
                    reply: None,
                    payload: Vec::new().into(),
                    headers: None,
                    status: None,
                    description: None,
                    length: 0,
                },
                invocation: invocation.clone(),
                queued: supervisor.metrics.queued(),
            })
            .unwrap_or_else(|_| panic!("failed to queue test invocation"));
        let engine = wasmtime::Engine::default();
        let metrics = MetricsRegistry::new().unwrap();
        let pools = crate::sql_pool::SqlPoolMap::new(1);
        let runtime = RuntimeState::new(
            engine.clone(),
            Arc::new(std::sync::RwLock::new(None)),
            metrics.clone(),
            pools,
            None,
            64 * 1024 * 1024,
        )
        .unwrap();
        let modules = ModuleRegistry::new(
            "http://localhost:3000".into(),
            engine,
            metrics.clone(),
            crate::oci::OciSource::new("localhost:5000", "localhost:5000").unwrap(),
        )
        .unwrap();
        let (_authority, synced_rx) = watch::channel(false);
        let (_client, client_rx) = watch::channel(None);
        let context = DispatchContext {
            state: Arc::new(runtime),
            app_registry: AppRegistry::new(),
            module_registry: modules,
            metrics_registry: metrics,
            client_rx,
            synced_rx,
        };
        let (shutdown, shutdown_rx) = broadcast::channel(1);
        let running = tokio::spawn(async move {
            supervisor.run(context, shutdown_rx).await;
            supervisor
        });
        bounded(invocation.cancelled()).await;
        shutdown.send(()).unwrap();
        let supervisor = bounded(running).await.unwrap();
        assert!(supervisor.jobs.is_empty());
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_queue_exits_total",
                Some(("outcome", "error"))
            ),
            1.0
        );
        assert_eq!(
            metric(&registry, "wasm_host_dispatch_tasks_finished_total", None),
            0.0
        );
    }

    #[tokio::test]
    async fn authority_loss_cancels_execution_and_queue_while_retaining_cleanup_capacity() {
        let (mut supervisor, sender, registry) = supervisor();
        let active = Invocation::with_metrics(Duration::from_secs(30), supervisor.metrics.clone());
        let (cleanup_entered, cleanup_started) = oneshot::channel();
        let (release_cleanup, cleanup_released) = oneshot::channel();
        active.spawn_cleanup(async move {
            cleanup_entered.send(()).unwrap();
            bounded(cleanup_released).await.unwrap();
        });
        bounded(cleanup_started).await.unwrap();
        let (execution_entered, execution_started) = oneshot::channel();
        let permit = supervisor.slots.clone().acquire_owned().await.unwrap();
        supervisor.spawn_execution(
            job(&supervisor, active.clone()),
            permit,
            move |invocation| {
                execution_entered.send(()).unwrap();
                tokio::runtime::Handle::current().block_on(invocation.cancelled());
                Ok(Some(b"revoked result".to_vec()))
            },
        );
        bounded(execution_started).await.unwrap();
        let queued = Invocation::with_metrics(Duration::from_secs(30), supervisor.metrics.clone());
        sender
            .try_send(InvocationMessage {
                message: async_nats::Message {
                    subject: "fn.test".into(),
                    reply: None,
                    payload: Vec::new().into(),
                    headers: None,
                    status: None,
                    description: None,
                    length: 0,
                },
                invocation: queued.clone(),
                queued: supervisor.metrics.queued(),
            })
            .unwrap_or_else(|_| panic!("failed to queue test invocation"));
        supervisor.cancel_untrusted_work();
        assert!(active.check().is_err());
        assert!(queued.check().is_err());
        cancel_pending_collection(&mut supervisor).await;
        assert_eq!(supervisor.slots.available_permits(), 0);
        assert_eq!(supervisor.jobs.len(), 1);
        release_cleanup.send(()).unwrap();
        collect_next(&mut supervisor).await;
        assert!(supervisor.tasks.is_empty());
        assert!(supervisor.jobs.is_empty());
        assert_eq!(supervisor.slots.available_permits(), 1);
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_tasks_finished_total",
                Some(("outcome", "cancelled"))
            ),
            1.0
        );
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_tasks_reaped_total",
                Some(("outcome", "cancelled"))
            ),
            1.0
        );
        bounded(supervisor.stop_and_drain()).await;
    }

    #[tokio::test]
    async fn cancelled_collection_retains_execution_until_guest_and_cleanup_exit() {
        let (mut supervisor, _sender, registry) = supervisor();
        let invocation =
            Invocation::with_metrics(Duration::from_secs(30), supervisor.metrics.clone());
        let (cleanup_entered_tx, cleanup_entered_rx) = oneshot::channel();
        let (cleanup_exit_tx, cleanup_exit_rx) = oneshot::channel();
        invocation.spawn_cleanup(async move {
            cleanup_entered_tx.send(()).unwrap();
            bounded(cleanup_exit_rx).await.unwrap();
        });
        let (guest_entered_tx, guest_entered_rx) = oneshot::channel();
        let (guest_exit_tx, guest_exit_rx) = oneshot::channel();
        let (guest_finished_tx, guest_finished_rx) = oneshot::channel();
        let permit = supervisor.slots.clone().acquire_owned().await.unwrap();
        supervisor.spawn_execution(job(&supervisor, invocation), permit, move |_| {
            guest_entered_tx.send(()).unwrap();
            tokio::runtime::Handle::current()
                .block_on(bounded(guest_exit_rx))
                .unwrap();
            guest_finished_tx.send(()).unwrap();
            Ok(None)
        });
        bounded(guest_entered_rx).await.unwrap();
        bounded(cleanup_entered_rx).await.unwrap();
        cancel_pending_collection(&mut supervisor).await;
        assert_eq!(supervisor.jobs.len(), 1);
        assert_eq!(supervisor.slots.available_permits(), 0);
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_work",
                Some(("state", "tracked"))
            ),
            1.0
        );

        guest_exit_tx.send(()).unwrap();
        bounded(guest_finished_rx).await.unwrap();
        cancel_pending_collection(&mut supervisor).await;
        assert_eq!(supervisor.jobs.len(), 1);
        assert_eq!(supervisor.slots.available_permits(), 0);
        cleanup_exit_tx.send(()).unwrap();
        collect_next(&mut supervisor).await;
        assert_eq!(supervisor.slots.available_permits(), 1);
        assert_eq!(supervisor.jobs.len(), 1);
        collect_next(&mut supervisor).await;
        assert!(supervisor.jobs.is_empty());
        assert!(supervisor.tasks.is_empty());
        assert_eq!(metric(&registry, "wasm_host_dispatch_work", None), 0.0);
        assert_eq!(
            metric(&registry, "wasm_host_dispatch_tasks_reaped_total", None),
            1.0
        );
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_accounting_errors_total",
                None
            ),
            0.0
        );
        bounded(supervisor.stop_and_drain()).await;
    }

    #[tokio::test]
    async fn guest_panic_cancels_work_and_waits_for_cleanup_before_collection() {
        let (mut supervisor, _sender, registry) = supervisor();
        let invocation =
            Invocation::with_metrics(Duration::from_secs(30), supervisor.metrics.clone());
        let cancellation = invocation.clone();
        let (cleanup_entered_tx, cleanup_entered_rx) = oneshot::channel();
        let (cleanup_exit_tx, cleanup_exit_rx) = oneshot::channel();
        invocation.spawn_cleanup(async move {
            cleanup_entered_tx.send(()).unwrap();
            bounded(cleanup_exit_rx).await.unwrap();
        });
        let permit = supervisor.slots.clone().acquire_owned().await.unwrap();
        supervisor.spawn_execution(job(&supervisor, invocation), permit, |_| {
            panic!("guest execution panic")
        });
        bounded(cleanup_entered_rx).await.unwrap();
        bounded(cancellation.cancelled()).await;
        assert!(cancellation.check().is_err());
        cancel_pending_collection(&mut supervisor).await;
        assert_eq!(supervisor.jobs.len(), 1);
        assert_eq!(supervisor.slots.available_permits(), 0);
        cleanup_exit_tx.send(()).unwrap();
        let result = bounded(supervisor.tasks.join_next_with_id()).await.unwrap();
        assert!(matches!(&result, Err(error) if error.is_panic()));
        supervisor.collect(result);
        collect_next(&mut supervisor).await;
        assert!(supervisor.jobs.is_empty());
        assert_eq!(supervisor.slots.available_permits(), 1);
        assert_eq!(metric(&registry, "wasm_host_dispatch_work", None), 0.0);
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_tasks_finished_total",
                Some(("outcome", "panic"))
            ),
            1.0
        );
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_tasks_reaped_total",
                Some(("outcome", "panic"))
            ),
            1.0
        );
        bounded(supervisor.stop_and_drain()).await;
    }

    #[tokio::test]
    async fn successful_completion_stays_successful_when_collection_passes_deadline() {
        let (mut supervisor, _sender, registry) = supervisor();
        let invocation =
            Invocation::with_metrics(Duration::from_secs(30), supervisor.metrics.clone());
        let cancellation = invocation.clone();
        let permit = supervisor.slots.clone().acquire_owned().await.unwrap();
        supervisor.spawn_execution(job(&supervisor, invocation), permit, |_| Ok(None));
        let result = bounded(supervisor.tasks.join_next_with_id()).await.unwrap();
        assert_eq!(supervisor.jobs.len(), 1);
        assert_eq!(supervisor.slots.available_permits(), 1);
        tokio::time::pause();
        tokio::time::advance(Duration::from_secs(31)).await;
        assert!(cancellation.check().is_err());
        supervisor.collect(result);
        collect_next(&mut supervisor).await;
        assert!(supervisor.jobs.is_empty());
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_tasks_finished_total",
                Some(("outcome", "success"))
            ),
            1.0
        );
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_tasks_finished_total",
                Some(("outcome", "timeout"))
            ),
            0.0
        );
        assert_eq!(metric(&registry, "wasm_host_dispatch_work", None), 0.0);
        bounded(supervisor.stop_and_drain()).await;
    }

    #[tokio::test]
    async fn replies_retain_task_capacity_through_stalls_errors_and_panics() {
        for (reply_outcome, finished_outcome, reaped_outcome) in [
            (Some(ReplyOutcome::Sent), "success", "ok"),
            (Some(ReplyOutcome::Failed), "error", "ok"),
            (Some(ReplyOutcome::Unavailable), "error", "ok"),
            (Some(ReplyOutcome::TimedOut), "error", "ok"),
            (None, "panic", "panic"),
        ] {
            let (mut supervisor, _sender, registry) = supervisor();
            let (entered_tx, entered_rx) = oneshot::channel();
            let (exit_tx, exit_rx) = oneshot::channel();
            supervisor.spawn_reply(
                supervisor.metrics.task_started(),
                "success",
                "ok",
                async move {
                    entered_tx.send(()).unwrap();
                    bounded(exit_rx).await.unwrap();
                    reply_outcome.unwrap_or_else(|| panic!("reply task panic"))
                },
            );
            bounded(entered_rx).await.unwrap();
            cancel_pending_collection(&mut supervisor).await;
            assert_eq!(supervisor.jobs.len(), supervisor.max_tracked);
            assert_eq!(
                metric(
                    &registry,
                    "wasm_host_dispatch_work",
                    Some(("state", "tracked"))
                ),
                1.0
            );
            exit_tx.send(()).unwrap();
            collect_next(&mut supervisor).await;
            assert!(supervisor.jobs.is_empty());
            assert!(supervisor.tasks.is_empty());
            assert_eq!(metric(&registry, "wasm_host_dispatch_work", None), 0.0);
            assert_eq!(
                metric(
                    &registry,
                    "wasm_host_dispatch_tasks_finished_total",
                    Some(("outcome", finished_outcome))
                ),
                1.0
            );
            assert_eq!(
                metric(
                    &registry,
                    "wasm_host_dispatch_tasks_reaped_total",
                    Some(("outcome", reaped_outcome))
                ),
                1.0
            );
            assert_eq!(
                metric(&registry, "wasm_host_dispatch_tasks_reaped_total", None),
                1.0
            );
            assert_eq!(
                metric(
                    &registry,
                    "wasm_host_dispatch_accounting_errors_total",
                    None
                ),
                0.0
            );
            bounded(supervisor.stop_and_drain()).await;
        }
    }

    #[tokio::test]
    async fn aborted_reply_is_collected_as_cancelled() {
        let (mut supervisor, _sender, registry) = supervisor();
        let (entered_tx, entered_rx) = oneshot::channel();
        supervisor.spawn_reply(
            supervisor.metrics.task_started(),
            "success",
            "ok",
            async move {
                entered_tx.send(()).unwrap();
                std::future::pending::<ReplyOutcome>().await
            },
        );
        bounded(entered_rx).await.unwrap();
        supervisor.tasks.abort_all();
        collect_next(&mut supervisor).await;
        assert!(supervisor.jobs.is_empty());
        assert_eq!(metric(&registry, "wasm_host_dispatch_work", None), 0.0);
        for name in [
            "wasm_host_dispatch_tasks_finished_total",
            "wasm_host_dispatch_tasks_reaped_total",
        ] {
            assert_eq!(metric(&registry, name, Some(("outcome", "cancelled"))), 1.0);
        }
        bounded(supervisor.stop_and_drain()).await;
    }

    #[tokio::test]
    async fn shutdown_cancels_queued_messages_without_admitting_them() {
        let (mut supervisor, sender, registry) = supervisor();
        let invocation =
            Invocation::with_metrics(Duration::from_secs(30), supervisor.metrics.clone());
        sender
            .try_send(InvocationMessage {
                message: async_nats::Message {
                    subject: "fn.test".into(),
                    reply: None,
                    payload: Vec::new().into(),
                    headers: None,
                    status: None,
                    description: None,
                    length: 0,
                },
                invocation: invocation.clone(),
                queued: supervisor.metrics.queued(),
            })
            .unwrap_or_else(|_| panic!("failed to queue test invocation"));
        bounded(supervisor.stop_and_drain()).await;
        assert!(sender.is_closed());
        assert!(invocation.check().is_err());
        assert!(supervisor.jobs.is_empty());
        assert!(supervisor.tasks.is_empty());
        assert_eq!(supervisor.slots.available_permits(), 1);
        assert_eq!(metric(&registry, "wasm_host_dispatch_work", None), 0.0);
        assert_eq!(
            metric(&registry, "wasm_host_dispatch_tasks_finished_total", None),
            0.0
        );
        assert_eq!(
            metric(&registry, "wasm_host_dispatch_tasks_reaped_total", None),
            0.0
        );
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_queue_exits_total",
                Some(("outcome", "shutdown"))
            ),
            1.0
        );
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_admissions_after_stop_total",
                None
            ),
            0.0
        );
        bounded(supervisor.stop_and_drain()).await;
        assert_eq!(
            metric(
                &registry,
                "wasm_host_dispatch_queue_exits_total",
                Some(("outcome", "shutdown"))
            ),
            1.0
        );
    }
}
