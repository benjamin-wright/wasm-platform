use std::{
    sync::{Arc, Mutex, MutexGuard},
    time::Instant,
};

use anyhow::Result;
use prometheus::{
    Histogram, HistogramOpts, IntCounter, IntCounterVec, IntGauge, IntGaugeVec, Opts, Registry,
};

const WORK_STATES: [&str; 5] = [
    "queued",
    "tracked",
    "completed_uncollected",
    "running",
    "terminating",
];
const CAPACITIES: [&str; 3] = ["queued", "execution", "tracked"];
const OUTCOMES: [&str; 7] = [
    "success",
    "timeout",
    "error",
    "panic",
    "cancelled",
    "rejected",
    "other",
];
const QUEUE_OUTCOMES: [&str; 7] = [
    "admitted",
    "expired",
    "unknown_route",
    "module_unavailable",
    "error",
    "shutdown",
    "other",
];
const DURATION_BUCKETS: [f64; 12] = [
    0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0, 10.0, 30.0, 60.0, 120.0,
];

#[derive(Default)]
struct WorkCounts {
    queued: usize,
    tracked: usize,
    completed_uncollected: usize,
    running: usize,
    terminating: usize,
    limits: Option<[usize; 3]>,
    high_water: [usize; 3],
    stopped_at: Option<Instant>,
    drain_finished: bool,
}

struct Inner {
    counts: Mutex<WorkCounts>,
    work: IntGaugeVec,
    limits: IntGaugeVec,
    high_water: IntGaugeVec,
    capacity_breaches: IntCounterVec,
    accounting_errors: IntCounterVec,
    admissions_after_stop: IntCounter,
    finished: IntCounterVec,
    reaped: IntCounterVec,
    queue_exits: IntCounterVec,
    queue_wait: Histogram,
    execution_duration: Histogram,
    cancellation_to_exit: Histogram,
    drain_duration: Histogram,
    draining: IntGauge,
}

/// Host-wide dispatch accounting owned by queue, task and execution lifetimes.
#[derive(Clone)]
pub(crate) struct DispatchMetrics {
    inner: Arc<Inner>,
}

impl DispatchMetrics {
    pub(crate) fn new(registry: &Registry) -> Result<Self> {
        let work = IntGaugeVec::new(
            Opts::new(
                "wasm_host_dispatch_work",
                "Work retained at each dispatch stage; tracked includes active and completed tasks",
            ),
            &["state"],
        )?;
        let limits = IntGaugeVec::new(
            Opts::new(
                "wasm_host_dispatch_limit",
                "Configured host dispatch capacity",
            ),
            &["kind"],
        )?;
        let high_water = IntGaugeVec::new(
            Opts::new(
                "wasm_host_dispatch_high_water",
                "Highest observed dispatch occupancy since process startup",
            ),
            &["kind"],
        )?;
        let capacity_breaches = IntCounterVec::new(
            Opts::new(
                "wasm_host_dispatch_capacity_breaches_total",
                "Occupancy above a dispatch limit on admission or configuration",
            ),
            &["kind"],
        )?;
        let accounting_errors = IntCounterVec::new(
            Opts::new(
                "wasm_host_dispatch_accounting_errors_total",
                "Unbalanced dispatch lifetime accounting",
            ),
            &["state"],
        )?;
        let admissions_after_stop = IntCounter::new(
            "wasm_host_dispatch_admissions_after_stop_total",
            "Tasks admitted after dispatch intake stopped",
        )?;
        let finished = IntCounterVec::new(
            Opts::new(
                "wasm_host_dispatch_tasks_finished_total",
                "Task wrappers that exited, before collection",
            ),
            &["outcome"],
        )?;
        let reaped = IntCounterVec::new(
            Opts::new(
                "wasm_host_dispatch_tasks_reaped_total",
                "Task records collected by dispatch",
            ),
            &["outcome"],
        )?;
        let queue_exits = IntCounterVec::new(
            Opts::new(
                "wasm_host_dispatch_queue_exits_total",
                "Messages that leave the host-owned waiting backlog",
            ),
            &["outcome"],
        )?;
        let queue_wait = duration_histogram(
            "wasm_host_dispatch_queue_wait_seconds",
            "Time retained waiting for invocation admission or rejection",
        )?;
        let execution_duration = duration_histogram(
            "wasm_host_dispatch_execution_seconds",
            "Execution lifetime through guest exit and backend cleanup",
        )?;
        let cancellation_to_exit = duration_histogram(
            "wasm_host_dispatch_cancellation_to_exit_seconds",
            "Time from observed cancellation until execution and cleanup exit",
        )?;
        let drain_duration = duration_histogram(
            "wasm_host_dispatch_drain_seconds",
            "Time from stopping intake until execution and task drain finishes",
        )?;
        let draining = IntGauge::new(
            "wasm_host_dispatch_draining",
            "Whether dispatch has stopped intake and is still draining work",
        )?;

        for state in WORK_STATES {
            work.with_label_values(&[state]);
            accounting_errors.with_label_values(&[state]);
        }
        for kind in CAPACITIES {
            limits.with_label_values(&[kind]);
            high_water.with_label_values(&[kind]);
            capacity_breaches.with_label_values(&[kind]);
        }
        for outcome in OUTCOMES {
            finished.with_label_values(&[outcome]);
        }
        for outcome in ["ok", "panic", "cancelled", "other"] {
            reaped.with_label_values(&[outcome]);
        }
        for outcome in QUEUE_OUTCOMES {
            queue_exits.with_label_values(&[outcome]);
        }

        registry.register(Box::new(work.clone()))?;
        registry.register(Box::new(limits.clone()))?;
        registry.register(Box::new(high_water.clone()))?;
        registry.register(Box::new(capacity_breaches.clone()))?;
        registry.register(Box::new(accounting_errors.clone()))?;
        registry.register(Box::new(admissions_after_stop.clone()))?;
        registry.register(Box::new(finished.clone()))?;
        registry.register(Box::new(reaped.clone()))?;
        registry.register(Box::new(queue_exits.clone()))?;
        registry.register(Box::new(queue_wait.clone()))?;
        registry.register(Box::new(execution_duration.clone()))?;
        registry.register(Box::new(cancellation_to_exit.clone()))?;
        registry.register(Box::new(drain_duration.clone()))?;
        registry.register(Box::new(draining.clone()))?;

        Ok(Self {
            inner: Arc::new(Inner {
                counts: Mutex::new(WorkCounts::default()),
                work,
                limits,
                high_water,
                capacity_breaches,
                accounting_errors,
                admissions_after_stop,
                finished,
                reaped,
                queue_exits,
                queue_wait,
                execution_duration,
                cancellation_to_exit,
                drain_duration,
                draining,
            }),
        })
    }

    pub(crate) fn configure(&self, queue_limit: usize, execution_limit: usize, task_limit: usize) {
        let mut counts = self.counts();
        let limits = [queue_limit, execution_limit, task_limit];
        counts.limits = Some(limits);
        for (index, (kind, limit)) in CAPACITIES.into_iter().zip(limits).enumerate() {
            self.inner
                .limits
                .with_label_values(&[kind])
                .set(as_gauge(limit));
            let occupancy = match index {
                0 => counts.queued,
                1 => counts.running + counts.terminating,
                _ => counts.tracked,
            };
            if occupancy > limit {
                self.inner
                    .capacity_breaches
                    .with_label_values(&[kind])
                    .inc();
            }
        }
    }

    pub(crate) fn queued(&self) -> QueuedWork {
        let mut counts = self.counts();
        counts.queued += 1;
        self.publish_counts(&counts);
        self.record_capacity(&mut counts, 0);
        QueuedWork {
            metrics: self.clone(),
            started_at: Instant::now(),
            outcome: "shutdown",
        }
    }

    pub(crate) fn task_started(&self) -> TaskRecord {
        let mut counts = self.counts();
        counts.tracked += 1;
        if counts.stopped_at.is_some() {
            self.inner.admissions_after_stop.inc();
        }
        self.publish_counts(&counts);
        self.record_capacity(&mut counts, 2);
        TaskRecord {
            metrics: self.clone(),
            outcome: "cancelled",
        }
    }

    pub(crate) fn task_reaped(&self, outcome: &'static str) {
        let mut counts = self.counts();
        self.decrement(&mut counts.tracked, "tracked");
        self.decrement(&mut counts.completed_uncollected, "completed_uncollected");
        self.publish_counts(&counts);
        let outcome = match outcome {
            "ok" => "ok",
            "panic" => "panic",
            "cancelled" => "cancelled",
            _ => "other",
        };
        self.inner.reaped.with_label_values(&[outcome]).inc();
    }

    pub(crate) fn execution_started(&self) -> Execution {
        let mut counts = self.counts();
        counts.running += 1;
        self.publish_counts(&counts);
        self.record_capacity(&mut counts, 1);
        Execution {
            metrics: self.clone(),
            started_at: Instant::now(),
            terminating_at: None,
        }
    }

    pub(crate) fn dispatch_stopped(&self) {
        let mut counts = self.counts();
        if counts.stopped_at.is_none() {
            counts.stopped_at = Some(Instant::now());
            self.inner.draining.set(1);
        }
    }

    pub(crate) fn drain_finished(&self) {
        let mut counts = self.counts();
        if let Some(stopped_at) = counts.stopped_at
            && !counts.drain_finished
        {
            counts.drain_finished = true;
            self.inner
                .drain_duration
                .observe(stopped_at.elapsed().as_secs_f64());
            self.inner.draining.set(0);
            tracing::info!(
                drain_seconds = stopped_at.elapsed().as_secs_f64(),
                queued = counts.queued,
                tracked = counts.tracked,
                completed_uncollected = counts.completed_uncollected,
                running = counts.running,
                terminating = counts.terminating,
                queued_high_water = counts.high_water[0],
                execution_high_water = counts.high_water[1],
                tracked_high_water = counts.high_water[2],
                "dispatch drain summary"
            );
        }
    }

    fn counts(&self) -> MutexGuard<'_, WorkCounts> {
        self.inner
            .counts
            .lock()
            .unwrap_or_else(|error| error.into_inner())
    }

    fn publish_counts(&self, counts: &WorkCounts) {
        let values = [
            counts.queued,
            counts.tracked,
            counts.completed_uncollected,
            counts.running,
            counts.terminating,
        ];
        for (state, value) in WORK_STATES.into_iter().zip(values) {
            self.inner
                .work
                .with_label_values(&[state])
                .set(as_gauge(value));
        }
    }

    fn record_capacity(&self, counts: &mut WorkCounts, kind: usize) {
        let value = match kind {
            0 => counts.queued,
            1 => counts.running + counts.terminating,
            _ => counts.tracked,
        };
        if value > counts.high_water[kind] {
            counts.high_water[kind] = value;
            self.inner
                .high_water
                .with_label_values(&[CAPACITIES[kind]])
                .set(as_gauge(value));
        }
        if counts.limits.is_some_and(|limits| value > limits[kind]) {
            self.inner
                .capacity_breaches
                .with_label_values(&[CAPACITIES[kind]])
                .inc();
        }
    }

    fn decrement(&self, count: &mut usize, state: &'static str) {
        if let Some(value) = count.checked_sub(1) {
            *count = value;
        } else {
            self.inner
                .accounting_errors
                .with_label_values(&[state])
                .inc();
        }
    }
}

/// Retain alongside a message until it is admitted, rejected or dropped.
pub(crate) struct QueuedWork {
    metrics: DispatchMetrics,
    started_at: Instant,
    outcome: &'static str,
}

impl QueuedWork {
    pub(crate) fn set_outcome(&mut self, outcome: &'static str) {
        self.outcome = if QUEUE_OUTCOMES.contains(&outcome) {
            outcome
        } else {
            "other"
        };
    }
}

impl Drop for QueuedWork {
    fn drop(&mut self) {
        let mut counts = self.metrics.counts();
        self.metrics.decrement(&mut counts.queued, "queued");
        self.metrics.publish_counts(&counts);
        self.metrics
            .inner
            .queue_wait
            .observe(self.started_at.elapsed().as_secs_f64());
        self.metrics
            .inner
            .queue_exits
            .with_label_values(&[self.outcome])
            .inc();
    }
}

/// Retain inside a spawned task; collect its record with `task_reaped`.
pub(crate) struct TaskRecord {
    metrics: DispatchMetrics,
    outcome: &'static str,
}

impl TaskRecord {
    pub(crate) fn set_outcome(&mut self, outcome: &'static str) {
        self.outcome = bounded_outcome(outcome);
    }
}

impl Drop for TaskRecord {
    fn drop(&mut self) {
        let mut counts = self.metrics.counts();
        counts.completed_uncollected += 1;
        self.metrics.publish_counts(&counts);
        let outcome = if std::thread::panicking() {
            "panic"
        } else {
            self.outcome
        };
        self.metrics
            .inner
            .finished
            .with_label_values(&[outcome])
            .inc();
    }
}

/// Retain with execution ownership until the guest and all backend cleanup exit.
pub(crate) struct Execution {
    metrics: DispatchMetrics,
    started_at: Instant,
    terminating_at: Option<Instant>,
}

impl Execution {
    pub(crate) fn terminating(&mut self) {
        if self.terminating_at.is_none() {
            let mut counts = self.metrics.counts();
            self.metrics.decrement(&mut counts.running, "running");
            counts.terminating += 1;
            self.metrics.publish_counts(&counts);
            self.terminating_at = Some(Instant::now());
        }
    }
}

impl Drop for Execution {
    fn drop(&mut self) {
        let mut counts = self.metrics.counts();
        if let Some(terminating_at) = self.terminating_at {
            self.metrics
                .decrement(&mut counts.terminating, "terminating");
            self.metrics
                .inner
                .cancellation_to_exit
                .observe(terminating_at.elapsed().as_secs_f64());
        } else {
            self.metrics.decrement(&mut counts.running, "running");
        }
        self.metrics.publish_counts(&counts);
        self.metrics
            .inner
            .execution_duration
            .observe(self.started_at.elapsed().as_secs_f64());
    }
}

fn duration_histogram(name: &str, help: &str) -> Result<Histogram> {
    Ok(Histogram::with_opts(
        HistogramOpts::new(name, help).buckets(DURATION_BUCKETS.to_vec()),
    )?)
}

fn bounded_outcome(outcome: &'static str) -> &'static str {
    match outcome {
        "ok" | "success" => "success",
        "timeout" | "error" | "panic" | "cancelled" | "rejected" => outcome,
        _ => "other",
    }
}

fn as_gauge(value: usize) -> i64 {
    i64::try_from(value).unwrap_or(i64::MAX)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn guards_account_for_completion_collection_and_cleanup() {
        let metrics = DispatchMetrics::new(&Registry::new()).unwrap();
        metrics.configure(1, 1, 1);
        let mut queued = metrics.queued();
        let mut task = metrics.task_started();
        let mut execution = metrics.execution_started();
        queued.set_outcome("admitted");
        drop(queued);
        execution.terminating();
        execution.terminating();
        assert_eq!(metrics.inner.work.with_label_values(&["running"]).get(), 0);
        assert_eq!(
            metrics.inner.work.with_label_values(&["terminating"]).get(),
            1
        );
        drop(execution);
        task.set_outcome("success");
        drop(task);
        assert_eq!(metrics.inner.work.with_label_values(&["tracked"]).get(), 1);
        assert_eq!(
            metrics
                .inner
                .work
                .with_label_values(&["completed_uncollected"])
                .get(),
            1
        );
        metrics.task_reaped("ok");
        for state in WORK_STATES {
            assert_eq!(metrics.inner.work.with_label_values(&[state]).get(), 0);
        }
        for kind in CAPACITIES {
            assert_eq!(metrics.inner.high_water.with_label_values(&[kind]).get(), 1);
            assert_eq!(
                metrics
                    .inner
                    .capacity_breaches
                    .with_label_values(&[kind])
                    .get(),
                0
            );
        }
        assert_eq!(metrics.inner.queue_wait.get_sample_count(), 1);
        assert_eq!(
            metrics
                .inner
                .queue_exits
                .with_label_values(&["admitted"])
                .get(),
            1
        );
        assert_eq!(metrics.inner.execution_duration.get_sample_count(), 1);
        assert_eq!(metrics.inner.cancellation_to_exit.get_sample_count(), 1);
    }

    #[test]
    fn brief_capacity_and_stop_violations_remain_visible() {
        let metrics = DispatchMetrics::new(&Registry::new()).unwrap();
        metrics.configure(1, 1, 1);
        let first = metrics.queued();
        let second = metrics.queued();
        drop(first);
        drop(second);
        metrics.dispatch_stopped();
        metrics.dispatch_stopped();
        drop(metrics.task_started());
        metrics.task_reaped("cancelled");
        metrics.drain_finished();
        metrics.drain_finished();
        assert_eq!(metrics.inner.work.with_label_values(&["queued"]).get(), 0);
        assert_eq!(
            metrics
                .inner
                .high_water
                .with_label_values(&["queued"])
                .get(),
            2
        );
        assert_eq!(
            metrics
                .inner
                .capacity_breaches
                .with_label_values(&["queued"])
                .get(),
            1
        );
        assert_eq!(metrics.inner.admissions_after_stop.get(), 1);
        assert_eq!(metrics.inner.draining.get(), 0);
        assert_eq!(metrics.inner.drain_duration.get_sample_count(), 1);
    }

    #[test]
    fn panicking_task_is_counted_before_its_record_is_collected() {
        let metrics = DispatchMetrics::new(&Registry::new()).unwrap();
        let task_metrics = metrics.clone();
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || {
            let _task = task_metrics.task_started();
            panic!("task panic");
        }));
        assert!(result.is_err());
        metrics.task_reaped("panic");
        assert_eq!(
            metrics.inner.finished.with_label_values(&["panic"]).get(),
            1
        );
        assert_eq!(metrics.inner.reaped.with_label_values(&["panic"]).get(), 1);
        assert_eq!(metrics.inner.work.with_label_values(&["tracked"]).get(), 0);
        assert_eq!(
            metrics
                .inner
                .work
                .with_label_values(&["completed_uncollected"])
                .get(),
            0
        );
    }
}
