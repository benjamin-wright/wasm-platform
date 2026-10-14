use std::{
    sync::{Arc, Mutex, MutexGuard},
    time::Instant,
};

use anyhow::Result;
use prometheus::{Histogram, HistogramOpts, IntCounterVec, IntGaugeVec, Opts, Registry};

const WORK_STATES: [&str; 5] = [
    "admission_wait",
    "queued",
    "preparing",
    "compiling",
    "terminating",
];
const CAPACITIES: [&str; 4] = ["queued", "active", "tracked", "compiling"];
const SUBMISSION_OUTCOMES: [&str; 4] = ["admitted", "timeout", "cancelled", "closed"];
const OUTCOMES: [&str; 5] = ["success", "error", "timeout", "cancelled", "panic"];
const DURATION_BUCKETS: [f64; 12] = [
    0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1.0, 5.0, 10.0, 30.0, 60.0, 120.0,
];

#[derive(Clone, Copy, PartialEq, Eq)]
enum WorkState {
    Queued,
    Preparing,
    Compiling,
    Terminating,
}

impl WorkState {
    fn index(self) -> usize {
        match self {
            Self::Queued => 1,
            Self::Preparing => 2,
            Self::Compiling => 3,
            Self::Terminating => 4,
        }
    }
}

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(crate) enum PreparationOutcome {
    Success,
    Error,
    Timeout,
    Cancelled,
    Panic,
}

impl PreparationOutcome {
    fn label(self) -> &'static str {
        match self {
            Self::Success => "success",
            Self::Error => "error",
            Self::Timeout => "timeout",
            Self::Cancelled => "cancelled",
            Self::Panic => "panic",
        }
    }
}

struct Counts {
    work: [usize; 5],
    compiling: usize,
    limits: [usize; 4],
    high_water: [usize; 4],
}

struct Inner {
    counts: Mutex<Counts>,
    work: IntGaugeVec,
    occupancy: IntGaugeVec,
    high_water: IntGaugeVec,
    capacity_breaches: IntCounterVec,
    accounting_errors: IntCounterVec,
    submissions: IntCounterVec,
    finished: IntCounterVec,
    compilation_finished: IntCounterVec,
    admission_wait: Histogram,
    queue_wait: Histogram,
    compilation_duration: Histogram,
    compilation_overrun: Histogram,
}

/// Host-wide reporting retained with preparation permits through actual worker exit.
#[derive(Clone)]
pub(crate) struct PreparationMetrics {
    inner: Arc<Inner>,
}

impl PreparationMetrics {
    pub(crate) fn new(
        registry: &Registry,
        record_limit: usize,
        active_limit: usize,
    ) -> Result<Self> {
        let work = IntGaugeVec::new(
            Opts::new(
                "wasm_host_preparation_work",
                "Retained module preparation work by lifecycle state",
            ),
            &["state"],
        )?;
        let limits = IntGaugeVec::new(
            Opts::new(
                "wasm_host_preparation_limit",
                "Configured preparation capacity; queued includes reserved records awaiting active admission",
            ),
            &["kind"],
        )?;
        let occupancy = IntGaugeVec::new(
            Opts::new(
                "wasm_host_preparation_occupancy",
                "Current preparation capacity use; compiling includes expired workers until actual compiler exit",
            ),
            &["kind"],
        )?;
        let high_water = IntGaugeVec::new(
            Opts::new(
                "wasm_host_preparation_high_water",
                "Highest preparation occupancy since process startup",
            ),
            &["kind"],
        )?;
        let capacity_breaches = IntCounterVec::new(
            Opts::new(
                "wasm_host_preparation_capacity_breaches_total",
                "Preparation admissions exceeding a configured capacity",
            ),
            &["kind"],
        )?;
        let accounting_errors = IntCounterVec::new(
            Opts::new(
                "wasm_host_preparation_accounting_errors_total",
                "Unbalanced or invalid preparation lifecycle accounting",
            ),
            &["state"],
        )?;
        let submissions = IntCounterVec::new(
            Opts::new(
                "wasm_host_preparation_submissions_total",
                "Preparation submissions leaving record admission",
            ),
            &["outcome"],
        )?;
        let finished = IntCounterVec::new(
            Opts::new(
                "wasm_host_preparation_finished_total",
                "Admitted preparation lifetimes that actually exited",
            ),
            &["outcome"],
        )?;
        let compilation_finished = IntCounterVec::new(
            Opts::new(
                "wasm_host_preparation_compilation_finished_total",
                "Started compilation workers that actually exited compilation",
            ),
            &["outcome"],
        )?;
        let admission_wait = duration_histogram(
            "wasm_host_preparation_admission_wait_seconds",
            "Time waiting for a retained preparation record",
        )?;
        let queue_wait = duration_histogram(
            "wasm_host_preparation_queue_wait_seconds",
            "Time an admitted preparation waits for active capacity",
        )?;
        let compilation_duration = duration_histogram(
            "wasm_host_preparation_compilation_seconds",
            "Actual blocking compilation worker duration including work after expiry",
        )?;
        let compilation_overrun = duration_histogram(
            "wasm_host_preparation_compilation_overrun_seconds",
            "Time from observed expiry or cancellation until compilation actually exits",
        )?;

        for state in WORK_STATES {
            work.with_label_values(&[state]);
            accounting_errors.with_label_values(&[state]);
        }
        for kind in CAPACITIES {
            limits.with_label_values(&[kind]);
            occupancy.with_label_values(&[kind]);
            high_water.with_label_values(&[kind]);
            capacity_breaches.with_label_values(&[kind]);
        }
        for outcome in SUBMISSION_OUTCOMES {
            submissions.with_label_values(&[outcome]);
        }
        for outcome in OUTCOMES {
            finished.with_label_values(&[outcome]);
            compilation_finished.with_label_values(&[outcome]);
        }

        registry.register(Box::new(work.clone()))?;
        registry.register(Box::new(limits.clone()))?;
        registry.register(Box::new(occupancy.clone()))?;
        registry.register(Box::new(high_water.clone()))?;
        registry.register(Box::new(capacity_breaches.clone()))?;
        registry.register(Box::new(accounting_errors.clone()))?;
        registry.register(Box::new(submissions.clone()))?;
        registry.register(Box::new(finished.clone()))?;
        registry.register(Box::new(compilation_finished.clone()))?;
        registry.register(Box::new(admission_wait.clone()))?;
        registry.register(Box::new(queue_wait.clone()))?;
        registry.register(Box::new(compilation_duration.clone()))?;
        registry.register(Box::new(compilation_overrun.clone()))?;

        let configured_limits = [record_limit, active_limit, record_limit, active_limit];
        for (kind, limit) in CAPACITIES.into_iter().zip(configured_limits) {
            limits.with_label_values(&[kind]).set(as_gauge(limit));
        }
        Ok(Self {
            inner: Arc::new(Inner {
                counts: Mutex::new(Counts {
                    work: [0; 5],
                    compiling: 0,
                    limits: configured_limits,
                    high_water: [0; 4],
                }),
                work,
                occupancy,
                high_water,
                capacity_breaches,
                accounting_errors,
                submissions,
                finished,
                compilation_finished,
                admission_wait,
                queue_wait,
                compilation_duration,
                compilation_overrun,
            }),
        })
    }

    pub(crate) fn submission(&self) -> Submission {
        let mut counts = self.counts();
        counts.work[0] += 1;
        self.publish_counts(&mut counts);
        Submission {
            metrics: self.clone(),
            started_at: Instant::now(),
            outcome: "cancelled",
            completed: false,
        }
    }

    fn counts(&self) -> MutexGuard<'_, Counts> {
        self.inner
            .counts
            .lock()
            .unwrap_or_else(|error| error.into_inner())
    }

    fn publish_counts(&self, counts: &mut Counts) {
        for (state, value) in WORK_STATES.into_iter().zip(counts.work) {
            self.inner
                .work
                .with_label_values(&[state])
                .set(as_gauge(value));
        }
        let active = counts.work[2..].iter().sum::<usize>();
        let values = [
            counts.work[1],
            active,
            counts.work[1] + active,
            counts.compiling,
        ];
        for (index, (kind, value)) in CAPACITIES.into_iter().zip(values).enumerate() {
            self.inner
                .occupancy
                .with_label_values(&[kind])
                .set(as_gauge(value));
            if value > counts.high_water[index] {
                counts.high_water[index] = value;
                self.inner
                    .high_water
                    .with_label_values(&[kind])
                    .set(as_gauge(value));
            }
        }
    }

    fn record_capacity(&self, counts: &Counts, kind: usize) {
        let active = counts.work[2..].iter().sum::<usize>();
        let value = match kind {
            0 => counts.work[1],
            1 => active,
            2 => counts.work[1] + active,
            _ => counts.compiling,
        };
        if value > counts.limits[kind] {
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
            self.accounting_error(state);
        }
    }

    fn accounting_error(&self, state: &'static str) {
        self.inner
            .accounting_errors
            .with_label_values(&[state])
            .inc();
    }

    fn transition(&self, from: WorkState, to: WorkState) {
        let mut counts = self.counts();
        self.decrement(&mut counts.work[from.index()], WORK_STATES[from.index()]);
        counts.work[to.index()] += 1;
        self.publish_counts(&mut counts);
        if to == WorkState::Preparing && from == WorkState::Queued {
            self.record_capacity(&counts, 1);
        }
    }
}

/// Retain during admission; dropping an unfinished submission reports cancellation.
pub(crate) struct Submission {
    metrics: PreparationMetrics,
    started_at: Instant,
    outcome: &'static str,
    completed: bool,
}

impl Submission {
    pub(crate) fn admitted(mut self) -> PreparationWork {
        self.outcome = "admitted";
        self.completed = true;
        let mut counts = self.metrics.counts();
        self.complete(&mut counts);
        counts.work[1] += 1;
        self.metrics.publish_counts(&mut counts);
        self.metrics.record_capacity(&counts, 0);
        self.metrics.record_capacity(&counts, 2);
        PreparationWork {
            control: PreparationControl {
                metrics: self.metrics.clone(),
                state: Arc::new(Mutex::new(PreparationState {
                    stage: Some(WorkState::Queued),
                    queued_at: Instant::now(),
                    compilation_started_at: None,
                    terminating_at: None,
                    cancellation: None,
                    outcome: PreparationOutcome::Cancelled,
                })),
            },
        }
    }

    pub(crate) fn timed_out(mut self) {
        self.outcome = "timeout";
    }

    pub(crate) fn closed(mut self) {
        self.outcome = "closed";
    }

    fn complete(&self, counts: &mut Counts) {
        self.metrics
            .decrement(&mut counts.work[0], "admission_wait");
        self.metrics
            .inner
            .admission_wait
            .observe(self.started_at.elapsed().as_secs_f64());
        self.metrics
            .inner
            .submissions
            .with_label_values(&[self.outcome])
            .inc();
    }
}

impl Drop for Submission {
    fn drop(&mut self) {
        if self.completed {
            return;
        }
        let mut counts = self.metrics.counts();
        self.complete(&mut counts);
        self.metrics.publish_counts(&mut counts);
    }
}

struct PreparationState {
    stage: Option<WorkState>,
    queued_at: Instant,
    compilation_started_at: Option<Instant>,
    terminating_at: Option<Instant>,
    cancellation: Option<PreparationOutcome>,
    outcome: PreparationOutcome,
}

/// A reporting handle marks abandoned work without retaining its capacity lifetime.
#[derive(Clone)]
pub(crate) struct PreparationControl {
    metrics: PreparationMetrics,
    state: Arc<Mutex<PreparationState>>,
}

impl PreparationControl {
    #[cfg(test)]
    pub(crate) fn timed_out(&self) {
        self.timed_out_at(Instant::now());
    }

    pub(crate) fn timed_out_at(&self, deadline: Instant) {
        self.terminate(PreparationOutcome::Timeout, deadline);
    }

    pub(crate) fn cancelled(&self) {
        self.terminate(PreparationOutcome::Cancelled, Instant::now());
    }

    fn state(&self) -> MutexGuard<'_, PreparationState> {
        self.state.lock().unwrap_or_else(|error| error.into_inner())
    }

    fn terminate(&self, outcome: PreparationOutcome, terminated_at: Instant) {
        let mut state = self.state();
        let Some(stage) = state.stage else {
            return;
        };
        if state.cancellation.is_some() {
            return;
        }
        state.cancellation = Some(outcome);
        if matches!(stage, WorkState::Preparing | WorkState::Compiling) {
            state.terminating_at = Some(terminated_at);
            self.metrics.transition(stage, WorkState::Terminating);
            state.stage = Some(WorkState::Terminating);
        }
    }

    fn compilation_finished(&self, state: &mut PreparationState, outcome: PreparationOutcome) {
        let Some(started_at) = state.compilation_started_at.take() else {
            self.metrics.accounting_error("compiling");
            return;
        };
        let outcome = state.cancellation.unwrap_or(outcome);
        self.metrics
            .inner
            .compilation_duration
            .observe(started_at.elapsed().as_secs_f64());
        self.metrics
            .inner
            .compilation_finished
            .with_label_values(&[outcome.label()])
            .inc();
        if let Some(terminating_at) = state.terminating_at.take() {
            self.metrics
                .inner
                .compilation_overrun
                .observe(terminating_at.elapsed().as_secs_f64());
        }
        let mut counts = self.metrics.counts();
        self.metrics.decrement(&mut counts.compiling, "compiling");
        self.metrics.publish_counts(&mut counts);
    }
}

/// Retain with admission permits, including inside the blocking compiler and result.
pub(crate) struct PreparationWork {
    control: PreparationControl,
}

impl PreparationWork {
    pub(crate) fn control(&self) -> PreparationControl {
        self.control.clone()
    }

    pub(crate) fn activate(&mut self) {
        let mut state = self.control.state();
        if state.stage != Some(WorkState::Queued) {
            self.control.metrics.accounting_error("queued");
            return;
        }
        self.control
            .metrics
            .inner
            .queue_wait
            .observe(state.queued_at.elapsed().as_secs_f64());
        self.control
            .metrics
            .transition(WorkState::Queued, WorkState::Preparing);
        state.stage = Some(WorkState::Preparing);
    }

    pub(crate) fn compiling(&mut self) {
        let mut state = self.control.state();
        if state.stage != Some(WorkState::Preparing)
            && !(state.stage == Some(WorkState::Terminating) && state.cancellation.is_some())
        {
            self.control.metrics.accounting_error("compiling");
            return;
        }
        state.compilation_started_at = Some(Instant::now());
        let next = if state.cancellation.is_some() {
            WorkState::Terminating
        } else {
            WorkState::Compiling
        };
        if state.stage == Some(WorkState::Preparing) {
            self.control.metrics.transition(WorkState::Preparing, next);
        }
        state.stage = Some(next);
        let mut counts = self.control.metrics.counts();
        counts.compiling += 1;
        self.control.metrics.publish_counts(&mut counts);
        self.control.metrics.record_capacity(&counts, 3);
    }

    pub(crate) fn compilation_finished(&mut self, outcome: PreparationOutcome) {
        let mut state = self.control.state();
        self.control.compilation_finished(&mut state, outcome);
        if state.stage == Some(WorkState::Compiling) {
            self.control
                .metrics
                .transition(WorkState::Compiling, WorkState::Preparing);
            state.stage = Some(WorkState::Preparing);
        }
    }

    pub(crate) fn finish(&mut self, outcome: PreparationOutcome) {
        self.control.state().outcome = outcome;
    }
}

impl Drop for PreparationWork {
    fn drop(&mut self) {
        let mut state = self.control.state();
        let Some(stage) = state.stage else {
            return;
        };
        let outcome = state.cancellation.unwrap_or(if std::thread::panicking() {
            PreparationOutcome::Panic
        } else {
            state.outcome
        });
        if state.compilation_started_at.is_some() {
            self.control.compilation_finished(&mut state, outcome);
        }
        if stage == WorkState::Queued {
            self.control
                .metrics
                .inner
                .queue_wait
                .observe(state.queued_at.elapsed().as_secs_f64());
        }
        let mut counts = self.control.metrics.counts();
        self.control
            .metrics
            .decrement(&mut counts.work[stage.index()], WORK_STATES[stage.index()]);
        self.control.metrics.publish_counts(&mut counts);
        self.control
            .metrics
            .inner
            .finished
            .with_label_values(&[outcome.label()])
            .inc();
        state.stage = None;
    }
}

fn duration_histogram(name: &str, help: &str) -> Result<Histogram> {
    Ok(Histogram::with_opts(
        HistogramOpts::new(name, help).buckets(DURATION_BUCKETS.to_vec()),
    )?)
}

fn as_gauge(value: usize) -> i64 {
    i64::try_from(value).unwrap_or(i64::MAX)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn work(metrics: &PreparationMetrics, state: &str) -> i64 {
        metrics.inner.work.with_label_values(&[state]).get()
    }

    fn assert_idle(metrics: &PreparationMetrics) {
        for state in WORK_STATES {
            assert_eq!(work(metrics, state), 0, "state {state}");
            assert_eq!(
                metrics
                    .inner
                    .accounting_errors
                    .with_label_values(&[state])
                    .get(),
                0
            );
        }
        assert_eq!(metrics.counts().compiling, 0);
        for kind in CAPACITIES {
            assert_eq!(metrics.inner.occupancy.with_label_values(&[kind]).get(), 0);
        }
    }

    #[test]
    fn successful_work_transfers_through_compilation_and_retained_result() {
        let metrics = PreparationMetrics::new(&Registry::new(), 17, 1).unwrap();
        let submission = metrics.submission();
        assert_eq!(work(&metrics, "admission_wait"), 1);
        let mut preparation = submission.admitted();
        assert_eq!(work(&metrics, "admission_wait"), 0);
        assert_eq!(work(&metrics, "queued"), 1);
        preparation.activate();
        preparation.compiling();
        assert_eq!(work(&metrics, "compiling"), 1);
        preparation.compilation_finished(PreparationOutcome::Success);
        assert_eq!(work(&metrics, "compiling"), 0);
        assert_eq!(work(&metrics, "preparing"), 1);
        preparation.finish(PreparationOutcome::Success);
        assert_eq!(
            metrics.inner.finished.with_label_values(&["success"]).get(),
            0
        );
        drop(preparation);
        assert_idle(&metrics);
        assert_eq!(
            metrics
                .inner
                .submissions
                .with_label_values(&["admitted"])
                .get(),
            1
        );
        assert_eq!(
            metrics.inner.finished.with_label_values(&["success"]).get(),
            1
        );
        assert_eq!(
            metrics
                .inner
                .compilation_finished
                .with_label_values(&["success"])
                .get(),
            1
        );
        assert_eq!(metrics.inner.admission_wait.get_sample_count(), 1);
        assert_eq!(metrics.inner.queue_wait.get_sample_count(), 1);
        assert_eq!(metrics.inner.compilation_duration.get_sample_count(), 1);
        assert_eq!(metrics.inner.compilation_overrun.get_sample_count(), 0);
        for kind in CAPACITIES {
            assert_eq!(metrics.inner.high_water.with_label_values(&[kind]).get(), 1);
        }
    }

    #[test]
    fn cancelled_and_expired_compilers_remain_owned_until_worker_exit() {
        for outcome in [PreparationOutcome::Timeout, PreparationOutcome::Cancelled] {
            let metrics = PreparationMetrics::new(&Registry::new(), 17, 1).unwrap();
            let mut preparation = metrics.submission().admitted();
            preparation.activate();
            preparation.compiling();
            let control = preparation.control();
            match outcome {
                PreparationOutcome::Timeout => control.timed_out(),
                _ => control.cancelled(),
            }
            control.cancelled();
            assert_eq!(work(&metrics, "compiling"), 0);
            assert_eq!(work(&metrics, "terminating"), 1);
            assert_eq!(metrics.counts().compiling, 1);
            assert_eq!(
                metrics
                    .inner
                    .occupancy
                    .with_label_values(&["compiling"])
                    .get(),
                1
            );
            assert_eq!(metrics.inner.compilation_duration.get_sample_count(), 0);
            preparation.compilation_finished(PreparationOutcome::Success);
            assert_eq!(
                metrics
                    .inner
                    .occupancy
                    .with_label_values(&["compiling"])
                    .get(),
                0
            );
            assert_eq!(
                metrics.inner.occupancy.with_label_values(&["active"]).get(),
                1
            );
            assert_eq!(
                metrics
                    .inner
                    .occupancy
                    .with_label_values(&["tracked"])
                    .get(),
                1
            );
            preparation.finish(PreparationOutcome::Success);
            assert_eq!(work(&metrics, "terminating"), 1);
            drop(preparation);
            control.timed_out();
            assert_idle(&metrics);
            assert_eq!(
                metrics
                    .inner
                    .finished
                    .with_label_values(&[outcome.label()])
                    .get(),
                1
            );
            assert_eq!(
                metrics
                    .inner
                    .compilation_finished
                    .with_label_values(&[outcome.label()])
                    .get(),
                1
            );
            assert_eq!(metrics.inner.compilation_overrun.get_sample_count(), 1);
        }
    }

    #[test]
    fn abandonment_before_active_capacity_and_before_worker_start_releases_accounting() {
        let metrics = PreparationMetrics::new(&Registry::new(), 17, 1).unwrap();
        metrics.submission().timed_out();
        metrics.submission().closed();
        drop(metrics.submission());
        let queued = metrics.submission().admitted();
        queued.control().timed_out();
        drop(queued);
        let mut preparing = metrics.submission().admitted();
        preparing.activate();
        preparing.control().cancelled();
        assert_eq!(work(&metrics, "preparing"), 0);
        assert_eq!(work(&metrics, "terminating"), 1);
        drop(preparing);
        assert_idle(&metrics);
        for outcome in ["timeout", "closed", "cancelled"] {
            assert_eq!(
                metrics
                    .inner
                    .submissions
                    .with_label_values(&[outcome])
                    .get(),
                1
            );
        }
        assert_eq!(
            metrics.inner.finished.with_label_values(&["timeout"]).get(),
            1
        );
        assert_eq!(
            metrics
                .inner
                .finished
                .with_label_values(&["cancelled"])
                .get(),
            1
        );
        assert_eq!(metrics.inner.compilation_duration.get_sample_count(), 0);
        assert_eq!(metrics.inner.queue_wait.get_sample_count(), 2);
    }

    #[test]
    fn cancellation_before_blocking_start_keeps_the_late_worker_accounted() {
        let metrics = PreparationMetrics::new(&Registry::new(), 17, 1).unwrap();
        let mut preparation = metrics.submission().admitted();
        preparation.activate();
        preparation.control().timed_out();
        preparation.compiling();
        assert_eq!(work(&metrics, "terminating"), 1);
        assert_eq!(metrics.counts().compiling, 1);
        preparation.compilation_finished(PreparationOutcome::Success);
        drop(preparation);
        assert_idle(&metrics);
        assert_eq!(
            metrics.inner.finished.with_label_values(&["timeout"]).get(),
            1
        );
        assert_eq!(
            metrics
                .inner
                .compilation_finished
                .with_label_values(&["timeout"])
                .get(),
            1
        );
        assert_eq!(metrics.inner.compilation_overrun.get_sample_count(), 1);
    }

    #[test]
    fn compilation_failure_and_panic_end_both_lifetimes() {
        let metrics = PreparationMetrics::new(&Registry::new(), 17, 1).unwrap();
        let mut preparation = metrics.submission().admitted();
        preparation.activate();
        preparation.compiling();
        preparation.compilation_finished(PreparationOutcome::Error);
        preparation.finish(PreparationOutcome::Error);
        drop(preparation);
        let task_metrics = metrics.clone();
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || {
            let mut preparation = task_metrics.submission().admitted();
            preparation.activate();
            preparation.compiling();
            panic!("worker panic");
        }));
        assert!(result.is_err());
        assert_idle(&metrics);
        for outcome in ["error", "panic"] {
            assert_eq!(
                metrics.inner.finished.with_label_values(&[outcome]).get(),
                1
            );
            assert_eq!(
                metrics
                    .inner
                    .compilation_finished
                    .with_label_values(&[outcome])
                    .get(),
                1
            );
        }
        assert_eq!(metrics.inner.compilation_duration.get_sample_count(), 2);
    }

    #[test]
    fn observed_timeout_remains_the_cause_of_a_later_worker_panic() {
        let metrics = PreparationMetrics::new(&Registry::new(), 17, 1).unwrap();
        let task_metrics = metrics.clone();
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(move || {
            let mut preparation = task_metrics.submission().admitted();
            preparation.activate();
            preparation.compiling();
            preparation.control().timed_out();
            panic!("worker panic after deadline");
        }));
        assert!(result.is_err());
        assert_idle(&metrics);
        assert_eq!(
            metrics.inner.finished.with_label_values(&["timeout"]).get(),
            1
        );
        assert_eq!(
            metrics
                .inner
                .compilation_finished
                .with_label_values(&["timeout"])
                .get(),
            1
        );
        assert_eq!(
            metrics.inner.finished.with_label_values(&["panic"]).get(),
            0
        );
        assert_eq!(metrics.inner.compilation_overrun.get_sample_count(), 1);
    }

    #[test]
    fn brief_excess_occupancy_retains_high_water_and_capacity_breaches() {
        let metrics = PreparationMetrics::new(&Registry::new(), 1, 1).unwrap();
        let mut first = metrics.submission().admitted();
        let mut second = metrics.submission().admitted();
        first.activate();
        second.activate();
        first.compiling();
        second.compiling();
        drop(first);
        drop(second);
        assert_idle(&metrics);
        for kind in CAPACITIES {
            assert_eq!(metrics.inner.high_water.with_label_values(&[kind]).get(), 2);
            assert_eq!(
                metrics
                    .inner
                    .capacity_breaches
                    .with_label_values(&[kind])
                    .get(),
                1
            );
        }
    }
}
