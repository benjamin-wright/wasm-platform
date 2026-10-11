use std::{
    future::Future,
    sync::{Arc, Mutex},
    time::Duration,
};

use tokio::{
    sync::{OwnedSemaphorePermit, watch},
    task::JoinHandle,
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
    cleanup: Mutex<Vec<JoinHandle<()>>>,
    metrics: Option<DispatchMetrics>,
    execution: Mutex<Option<Execution>>,
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
                cleanup: Mutex::new(Vec::new()),
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
    pub(crate) fn track(&self, task: JoinHandle<()>) {
        self.inner
            .cleanup
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(task);
    }

    pub(crate) async fn finish(&self) {
        let cleanup =
            std::mem::take(&mut *self.inner.cleanup.lock().unwrap_or_else(|e| e.into_inner()));
        for task in cleanup {
            if let Err(error) = task.await {
                tracing::error!(panicked = error.is_panic(), "backend cleanup failed");
            }
        }
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
pub(crate) async fn run_blocking<T: Send + 'static>(
    invocation: Invocation,
    permit: OwnedSemaphorePermit,
    work: impl FnOnce(Invocation) -> T + Send + 'static,
) -> Result<T, tokio::task::JoinError> {
    let execution = invocation.start_execution();
    tokio::task::spawn_blocking(move || {
        let _permit = permit;
        let _execution = execution;
        let result =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| work(invocation.clone())));
        if result.is_err() {
            invocation.cancel();
        }
        tokio::runtime::Handle::current().block_on(invocation.finish());
        match result {
            Ok(result) => result,
            Err(panic) => std::panic::resume_unwind(panic),
        }
    })
    .await
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
        let cleanup = tokio::spawn(async move {
            entered_tx.send(()).unwrap();
            exit_rx.await.unwrap();
        });
        invocation.track(cleanup);
        let task = tokio::spawn(run_blocking(invocation.clone(), permit, |_| ()));
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
        invocation.track(tokio::spawn(async {
            exit_rx.await.unwrap();
        }));
        let task = tokio::spawn(run_blocking(invocation.clone(), permit, |_| {
            panic!("test panic")
        }));
        invocation.cancelled().await;
        assert!(!task.is_finished());
        assert!(semaphore.try_acquire().is_err());
        exit_tx.send(()).unwrap();
        assert!(task.await.unwrap().unwrap_err().is_panic());
        assert_eq!(semaphore.available_permits(), 1);
    }
}
