use std::{
    future::Future,
    sync::{Arc, Mutex},
    time::Duration,
};

use tokio::{
    sync::{OwnedSemaphorePermit, watch},
    task::{JoinError, JoinSet},
    time::Instant,
};

use crate::dispatch_metrics::{DispatchMetrics, Execution};

/// One budget shared by queueing, guest execution and every host operation.
#[derive(Clone)]
pub(crate) struct Invocation {
    inner: Arc<InvocationState>,
}

struct InvocationState {
    deadline: Instant,
    cancelled: watch::Sender<bool>,
    cleanup: Mutex<CleanupTasks>,
    metrics: Option<DispatchMetrics>,
    execution: Mutex<Option<Execution>>,
}

#[derive(Default)]
struct CleanupTasks {
    tasks: JoinSet<()>,
    failure: Option<&'static str>,
}

impl CleanupTasks {
    fn collect(&mut self, result: Result<(), JoinError>) {
        if let Err(error) = result {
            let failure = if error.is_panic() {
                "backend cleanup panicked"
            } else {
                "backend cleanup was cancelled"
            };
            self.failure.get_or_insert(failure);
            tracing::error!(
                panicked = error.is_panic(),
                cancelled = error.is_cancelled(),
                "backend cleanup failed"
            );
        }
    }
}

impl Invocation {
    #[cfg(test)]
    pub(crate) fn new(timeout: Duration) -> Self {
        Self::build(timeout, None)
    }

    pub(crate) fn with_metrics(timeout: Duration, metrics: DispatchMetrics) -> Self {
        Self::build(timeout, Some(metrics))
    }

    fn build(timeout: Duration, metrics: Option<DispatchMetrics>) -> Self {
        Self {
            inner: Arc::new(InvocationState {
                deadline: Instant::now() + timeout,
                cancelled: watch::channel(false).0,
                cleanup: Mutex::new(CleanupTasks::default()),
                metrics,
                execution: Mutex::new(None),
            }),
        }
    }

    pub(crate) fn deadline(&self) -> Instant {
        self.inner.deadline
    }

    pub(crate) fn cancel(&self) {
        self.inner.cancelled.send_replace(true);
        if let Some(execution) = self
            .inner
            .execution
            .lock()
            .unwrap_or_else(|error| error.into_inner())
            .as_mut()
        {
            execution.terminating();
        }
    }

    fn start_execution(&self) -> ExecutionOwnership {
        if let Some(metrics) = &self.inner.metrics {
            *self
                .inner
                .execution
                .lock()
                .unwrap_or_else(|error| error.into_inner()) = Some(metrics.execution_started());
            let _ = self.check();
        }
        ExecutionOwnership(self.clone())
    }

    pub(crate) fn check(&self) -> Result<(), String> {
        if *self.inner.cancelled.borrow() || Instant::now() >= self.deadline() {
            self.cancel();
            Err("invocation cancelled or deadline exceeded".to_owned())
        } else {
            Ok(())
        }
    }

    pub(crate) async fn cancelled(&self) {
        let mut rx = self.inner.cancelled.subscribe();
        if *rx.borrow_and_update() {
            return;
        }
        tokio::select! {
            _ = tokio::time::sleep_until(self.deadline()) => {},
            _ = rx.wait_for(|cancelled| *cancelled) => {},
        }
        self.cancel();
    }

    pub(crate) async fn run<F: Future>(&self, future: F) -> Result<F::Output, String> {
        self.check()?;
        tokio::select! {
            biased;
            _ = self.cancelled() => Err("invocation cancelled or deadline exceeded".to_owned()),
            result = future => Ok(result),
        }
    }

    pub(crate) async fn acquire_permit(
        &self,
        semaphore: Arc<tokio::sync::Semaphore>,
    ) -> Result<OwnedSemaphorePermit, String> {
        self.run(semaphore.acquire_owned())
            .await?
            .map_err(|_| "invocation admission closed".to_owned())
    }

    /// Retain backend cleanup even when the guest's suspended future is dropped.
    pub(crate) fn spawn_cleanup(&self, future: impl Future<Output = ()> + Send + 'static) {
        let mut cleanup = self
            .inner
            .cleanup
            .lock()
            .unwrap_or_else(|error| error.into_inner());
        while let Some(result) = cleanup.tasks.try_join_next() {
            cleanup.collect(result);
        }
        cleanup.tasks.spawn(future);
    }

    pub(crate) async fn finish(&self) -> Result<(), String> {
        while let Some(result) = std::future::poll_fn(|cx| {
            self.inner
                .cleanup
                .lock()
                .unwrap_or_else(|error| error.into_inner())
                .tasks
                .poll_join_next(cx)
        })
        .await
        {
            self.inner
                .cleanup
                .lock()
                .unwrap_or_else(|error| error.into_inner())
                .collect(result);
        }
        self.inner
            .cleanup
            .lock()
            .unwrap_or_else(|error| error.into_inner())
            .failure
            .map_or(Ok(()), |failure| Err(failure.to_owned()))
    }
}

struct ExecutionOwnership(Invocation);

impl Drop for ExecutionOwnership {
    fn drop(&mut self) {
        self.0
            .inner
            .execution
            .lock()
            .unwrap_or_else(|error| error.into_inner())
            .take();
    }
}

/// Hold admission through execution exit, backend cleanup and panic unwinding.
pub(crate) struct BlockingInvocation {
    invocation: Invocation,
    _execution: ExecutionOwnership,
    _permit: OwnedSemaphorePermit,
}

impl BlockingInvocation {
    pub(crate) fn new(invocation: Invocation, permit: OwnedSemaphorePermit) -> Self {
        let execution = invocation.start_execution();
        Self {
            invocation,
            _execution: execution,
            _permit: permit,
        }
    }

    pub(crate) fn run<T>(
        self,
        work: impl FnOnce(Invocation) -> anyhow::Result<T>,
    ) -> anyhow::Result<T> {
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            work(self.invocation.clone())
        }));
        if result.is_err() {
            self.invocation.cancel();
        }
        let cleanup = tokio::runtime::Handle::current().block_on(self.invocation.finish());
        let _ = self.invocation.check();
        match result {
            Ok(result) => {
                cleanup.map_err(anyhow::Error::msg)?;
                result
            }
            Err(panic) => std::panic::resume_unwind(panic),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[tokio::test(start_paused = true)]
    async fn expired_work_is_never_polled() {
        let invocation = Invocation::new(Duration::from_secs(1));
        tokio::time::advance(Duration::from_secs(1)).await;
        let result = invocation.run(async { panic!("expired work ran") }).await;
        assert!(result.is_err());
    }

    #[tokio::test(start_paused = true)]
    async fn one_deadline_covers_successive_operations() {
        let invocation = Invocation::new(Duration::from_secs(3));
        assert!(
            invocation
                .run(tokio::time::sleep(Duration::from_secs(2)))
                .await
                .is_ok()
        );
        assert!(
            invocation
                .run(tokio::time::sleep(Duration::from_secs(2)))
                .await
                .is_err()
        );
        assert_eq!(Instant::now(), invocation.deadline());
    }

    #[tokio::test(start_paused = true)]
    async fn queued_work_expires_without_reacquiring_admission() {
        let semaphore = Arc::new(tokio::sync::Semaphore::new(1));
        let active = semaphore.clone().acquire_owned().await.unwrap();
        let queued = Invocation::new(Duration::from_secs(1));
        assert!(queued.acquire_permit(semaphore.clone()).await.is_err());
        assert_eq!(semaphore.available_permits(), 0);
        drop(active);
        assert!(queued.acquire_permit(semaphore.clone()).await.is_err());
        assert!(
            queued
                .run(async { panic!("expired queue entry started") })
                .await
                .is_err()
        );
        assert_eq!(semaphore.available_permits(), 1);
    }

    #[tokio::test]
    async fn explicit_cancellation_interrupts_admission_wait() {
        let semaphore = Arc::new(tokio::sync::Semaphore::new(0));
        let invocation = Invocation::new(Duration::from_secs(60));
        let cancellation = invocation.clone();
        let task = tokio::spawn(async move { invocation.acquire_permit(semaphore).await });
        cancellation.cancel();
        assert!(
            tokio::time::timeout(Duration::from_secs(1), task)
                .await
                .unwrap()
                .unwrap()
                .is_err()
        );
    }

    #[tokio::test]
    async fn permits_survive_cancellation_until_cleanup_exits() {
        let invocation = Invocation::new(Duration::from_secs(5));
        let semaphore = Arc::new(tokio::sync::Semaphore::new(1));
        let permit = semaphore.clone().acquire_owned().await.unwrap();
        let (entered_tx, entered_rx) = tokio::sync::oneshot::channel();
        let (exit_tx, exit_rx) = tokio::sync::oneshot::channel();
        invocation.spawn_cleanup(async move {
            entered_tx.send(()).unwrap();
            exit_rx.await.unwrap();
        });
        let blocking = BlockingInvocation::new(invocation.clone(), permit);
        let task = tokio::task::spawn_blocking(move || blocking.run(|_| Ok(())));
        entered_rx.await.unwrap();
        invocation.cancel();
        assert!(semaphore.try_acquire().is_err());
        exit_tx.send(()).unwrap();
        task.await.unwrap().unwrap();
        assert_eq!(semaphore.available_permits(), 1);
    }

    #[tokio::test]
    async fn a_panicking_execution_still_awaits_cleanup() {
        let invocation = Invocation::new(Duration::from_secs(5));
        let semaphore = Arc::new(tokio::sync::Semaphore::new(1));
        let permit = semaphore.clone().acquire_owned().await.unwrap();
        let (exit_tx, exit_rx) = tokio::sync::oneshot::channel();
        invocation.spawn_cleanup(async {
            exit_rx.await.unwrap();
        });
        let blocking = BlockingInvocation::new(invocation.clone(), permit);
        let task =
            tokio::task::spawn_blocking(move || blocking.run::<()>(|_| panic!("test panic")));
        invocation.cancelled().await;
        assert!(!task.is_finished());
        assert!(semaphore.try_acquire().is_err());
        exit_tx.send(()).unwrap();
        assert!(task.await.unwrap_err().is_panic());
        assert_eq!(semaphore.available_permits(), 1);
    }

    #[tokio::test]
    async fn cleanup_failure_waits_for_other_children_before_releasing_capacity() {
        let invocation = Invocation::new(Duration::from_secs(5));
        let semaphore = Arc::new(tokio::sync::Semaphore::new(1));
        let permit = semaphore.clone().acquire_owned().await.unwrap();
        let (panic_tx, panic_rx) = tokio::sync::oneshot::channel();
        invocation.spawn_cleanup(async move {
            panic_tx.send(()).unwrap();
            panic!("cleanup panic");
        });
        panic_rx.await.unwrap();
        tokio::task::yield_now().await;
        let (entered_tx, entered_rx) = tokio::sync::oneshot::channel();
        let (exit_tx, exit_rx) = tokio::sync::oneshot::channel();
        invocation.spawn_cleanup(async move {
            entered_tx.send(()).unwrap();
            exit_rx.await.unwrap();
        });
        assert_eq!(
            invocation.inner.cleanup.lock().unwrap().failure,
            Some("backend cleanup panicked")
        );
        let blocking = BlockingInvocation::new(invocation.clone(), permit);
        let task = tokio::task::spawn_blocking(move || blocking.run(|_| Ok(())));
        entered_rx.await.unwrap();
        assert!(!task.is_finished());
        assert!(semaphore.try_acquire().is_err());
        exit_tx.send(()).unwrap();
        let error = task.await.unwrap().unwrap_err();
        assert_eq!(error.to_string(), "backend cleanup panicked");
        assert_eq!(semaphore.available_permits(), 1);
        assert_eq!(invocation.finish().await.unwrap_err(), error.to_string());
    }

    #[tokio::test]
    async fn sequential_cleanup_registration_collects_completed_children() {
        let invocation = Invocation::new(Duration::from_secs(5));
        for _ in 0..10 {
            let (completed_tx, completed_rx) = tokio::sync::oneshot::channel();
            invocation.spawn_cleanup(async move {
                completed_tx.send(()).unwrap();
            });
            completed_rx.await.unwrap();
            tokio::task::yield_now().await;
            assert_eq!(invocation.inner.cleanup.lock().unwrap().tasks.len(), 1);
        }
        invocation.finish().await.unwrap();
        assert!(invocation.inner.cleanup.lock().unwrap().tasks.is_empty());
    }
}
