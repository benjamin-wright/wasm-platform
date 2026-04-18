use std::{
    collections::HashMap,
    sync::{Arc, RwLock},
};

use anyhow::Result;
use prometheus::{CounterVec, Encoder, GaugeVec, Opts, Registry, TextEncoder};

use crate::config::configsync;

// ── User-defined metric ───────────────────────────────────────────────────────

enum UserMetric {
    Counter(CounterVec),
    Gauge(GaugeVec),
}

struct UserMetricEntry {
    metric: UserMetric,
    /// Guest-declared label keys, excluding the host-injected app_name/app_namespace.
    label_keys: Vec<String>,
}

// ── Registry inner ────────────────────────────────────────────────────────────

struct Inner {
    registry: Registry,
    user_metrics: RwLock<HashMap<String, UserMetricEntry>>,
    // Platform counters
    compilations_total: CounterVec,
    events_received_total: CounterVec,
    messages_sent_total: CounterVec,
    kv_reads_total: CounterVec,
    kv_writes_total: CounterVec,
    http_requests_received_total: CounterVec,
    dropped_metric_calls_total: CounterVec,
}

// ── MetricsRegistry ───────────────────────────────────────────────────────────

/// Shared, thread-safe Prometheus metrics registry.
///
/// Owns all platform `CounterVec` handles and a map of user-defined metrics
/// pre-registered from Application `spec.metrics`.  Clone is cheap (Arc).
#[derive(Clone)]
pub struct MetricsRegistry {
    inner: Arc<Inner>,
}

impl MetricsRegistry {
    pub fn new() -> Result<Self> {
        let registry = Registry::new();

        let compilations_total = CounterVec::new(
            Opts::new(
                "wasm_host_module_compilations_total",
                "AOT compilations triggered on config arrival",
            ),
            &["app_name", "app_namespace", "result"],
        )?;
        let events_received_total = CounterVec::new(
            Opts::new(
                "wasm_host_events_received_total",
                "Invocation requests received before dispatch",
            ),
            &["app_name", "app_namespace", "trigger"],
        )?;
        let messages_sent_total = CounterVec::new(
            Opts::new(
                "wasm_host_messages_sent_total",
                "messaging.send host function calls",
            ),
            &["app_name", "app_namespace"],
        )?;
        let kv_reads_total = CounterVec::new(
            Opts::new(
                "wasm_host_kv_reads_total",
                "kv.get, kv.get-int, kv.incr, and kv.decr host function calls",
            ),
            &["app_name", "app_namespace"],
        )?;
        let kv_writes_total = CounterVec::new(
            Opts::new(
                "wasm_host_kv_writes_total",
                "kv.set, kv.set-int, kv.delete, kv.incr, and kv.decr host function calls",
            ),
            &["app_name", "app_namespace"],
        )?;
        let http_requests_received_total = CounterVec::new(
            Opts::new(
                "wasm_host_http_requests_received_total",
                "HTTP invocations completed; status is the guest response code",
            ),
            &["app_name", "app_namespace", "status"],
        )?;
        let dropped_metric_calls_total = CounterVec::new(
            Opts::new(
                "wasm_host_dropped_metric_calls_total",
                "Guest metric calls dropped due to schema violations",
            ),
            &["app_name", "app_namespace", "reason"],
        )?;

        registry.register(Box::new(compilations_total.clone()))?;
        registry.register(Box::new(events_received_total.clone()))?;
        registry.register(Box::new(messages_sent_total.clone()))?;
        registry.register(Box::new(kv_reads_total.clone()))?;
        registry.register(Box::new(kv_writes_total.clone()))?;
        registry.register(Box::new(http_requests_received_total.clone()))?;
        registry.register(Box::new(dropped_metric_calls_total.clone()))?;

        Ok(Self {
            inner: Arc::new(Inner {
                registry,
                user_metrics: RwLock::new(HashMap::new()),
                compilations_total,
                events_received_total,
                messages_sent_total,
                kv_reads_total,
                kv_writes_total,
                http_requests_received_total,
                dropped_metric_calls_total,
            }),
        })
    }

    /// Synchronise user-defined metric registrations with the current application set.
    ///
    /// Metrics whose owning application is no longer present are unregistered and
    /// removed.  Metrics for newly-seen applications are registered.  Metrics that
    /// are already registered and unchanged are left untouched.
    pub fn sync_user_metrics(
        &self,
        apps: Vec<(String, String, Vec<configsync::MetricDefinition>)>,
    ) -> Result<()> {
        // Build expected: metric_name -> MetricDefinition.
        let mut expected: HashMap<String, configsync::MetricDefinition> = HashMap::new();
        for (_namespace, _app_name, defs) in &apps {
            for def in defs {
                expected.insert(def.name.clone(), def.clone());
            }
        }

        let mut user_metrics = self
            .inner
            .user_metrics
            .write()
            .map_err(|_| anyhow::anyhow!("user_metrics lock poisoned"))?;

        // Unregister metrics no longer expected.
        let to_remove: Vec<String> = user_metrics
            .keys()
            .filter(|name| !expected.contains_key(*name))
            .cloned()
            .collect();
        for name in &to_remove {
            if let Some(entry) = user_metrics.get(name) {
                let result = match &entry.metric {
                    UserMetric::Counter(c) => self.inner.registry.unregister(Box::new(c.clone())),
                    UserMetric::Gauge(g) => self.inner.registry.unregister(Box::new(g.clone())),
                };
                if let Err(e) = result {
                    tracing::warn!(metric_name = %name, "failed to unregister user metric: {e}");
                }
            }
        }
        for name in &to_remove {
            user_metrics.remove(name);
        }

        // Register newly expected metrics.
        for (name, def) in expected {
            if user_metrics.contains_key(&name) {
                continue;
            }
            let guest_label_keys: Vec<String> = def.label_keys.clone();
            let mut all_label_keys: Vec<&str> = vec!["app_name", "app_namespace"];
            let guest_refs: Vec<&str> = guest_label_keys.iter().map(|s| s.as_str()).collect();
            all_label_keys.extend(guest_refs.iter().copied());

            let metric_type =
                configsync::MetricType::try_from(def.r#type).unwrap_or(configsync::MetricType::Counter);
            let entry = match metric_type {
                configsync::MetricType::Counter => {
                    let c = CounterVec::new(Opts::new(&name, &name), &all_label_keys)?;
                    self.inner.registry.register(Box::new(c.clone()))?;
                    UserMetricEntry {
                        metric: UserMetric::Counter(c),
                        label_keys: guest_label_keys,
                    }
                }
                configsync::MetricType::Gauge => {
                    let g = GaugeVec::new(Opts::new(&name, &name), &all_label_keys)?;
                    self.inner.registry.register(Box::new(g.clone()))?;
                    UserMetricEntry {
                        metric: UserMetric::Gauge(g),
                        label_keys: guest_label_keys,
                    }
                }
            };
            user_metrics.insert(name, entry);
        }

        Ok(())
    }

    /// Increment a user-defined counter.
    ///
    /// Returns a `(reason, metric_name)` error tuple on unknown metric or label
    /// mismatch so that the caller can increment `wasm_host_dropped_metric_calls_total`.
    pub fn counter_increment(
        &self,
        name: &str,
        app_name: &str,
        app_namespace: &str,
        labels: &[(String, String)],
    ) -> Result<(), DropReason> {
        let user_metrics = self
            .inner
            .user_metrics
            .read()
            .map_err(|_| DropReason::UnknownMetric)?;
        let entry = user_metrics
            .get(name)
            .ok_or(DropReason::UnknownMetric)?;
        let UserMetric::Counter(counter) = &entry.metric else {
            return Err(DropReason::WrongLabels);
        };
        let label_values = build_label_values(app_name, app_namespace, &entry.label_keys, labels)
            .map_err(|_| DropReason::WrongLabels)?;
        counter
            .with_label_values(&label_values.iter().map(|s| s.as_str()).collect::<Vec<_>>())
            .inc();
        Ok(())
    }

    /// Set a user-defined gauge.
    ///
    /// Returns a `DropReason` on unknown metric or label mismatch.
    pub fn gauge_set(
        &self,
        name: &str,
        value: f64,
        app_name: &str,
        app_namespace: &str,
        labels: &[(String, String)],
    ) -> Result<(), DropReason> {
        let user_metrics = self
            .inner
            .user_metrics
            .read()
            .map_err(|_| DropReason::UnknownMetric)?;
        let entry = user_metrics
            .get(name)
            .ok_or(DropReason::UnknownMetric)?;
        let UserMetric::Gauge(gauge) = &entry.metric else {
            return Err(DropReason::WrongLabels);
        };
        let label_values = build_label_values(app_name, app_namespace, &entry.label_keys, labels)
            .map_err(|_| DropReason::WrongLabels)?;
        gauge
            .with_label_values(&label_values.iter().map(|s| s.as_str()).collect::<Vec<_>>())
            .set(value);
        Ok(())
    }

    // ── Platform counter methods ──────────────────────────────────────────────

    pub fn record_compilation(&self, app_name: &str, app_namespace: &str, result: &str) {
        self.inner
            .compilations_total
            .with_label_values(&[app_name, app_namespace, result])
            .inc();
    }

    pub fn record_event(&self, app_name: &str, app_namespace: &str, trigger: &str) {
        self.inner
            .events_received_total
            .with_label_values(&[app_name, app_namespace, trigger])
            .inc();
    }

    pub fn record_message_sent(&self, app_name: &str, app_namespace: &str) {
        self.inner
            .messages_sent_total
            .with_label_values(&[app_name, app_namespace])
            .inc();
    }

    pub fn record_kv_read(&self, app_name: &str, app_namespace: &str) {
        self.inner
            .kv_reads_total
            .with_label_values(&[app_name, app_namespace])
            .inc();
    }

    pub fn record_kv_write(&self, app_name: &str, app_namespace: &str) {
        self.inner
            .kv_writes_total
            .with_label_values(&[app_name, app_namespace])
            .inc();
    }

    pub fn record_http_request(&self, app_name: &str, app_namespace: &str, status: u16) {
        self.inner
            .http_requests_received_total
            .with_label_values(&[app_name, app_namespace, &status.to_string()])
            .inc();
    }

    pub fn record_dropped_metric(&self, app_name: &str, app_namespace: &str, reason: DropReason) {
        self.inner
            .dropped_metric_calls_total
            .with_label_values(&[app_name, app_namespace, reason.as_str()])
            .inc();
    }

    /// Render all registered metrics in the Prometheus text exposition format.
    pub fn render(&self) -> Result<String> {
        let mut buffer = Vec::new();
        let encoder = TextEncoder::new();
        let metric_families = self.inner.registry.gather();
        encoder.encode(&metric_families, &mut buffer)?;
        Ok(String::from_utf8(buffer)?)
    }
}

// ── DropReason ────────────────────────────────────────────────────────────────

/// The reason a guest metric call was silently dropped.
#[derive(Debug, Clone, Copy)]
pub enum DropReason {
    UnknownMetric,
    WrongLabels,
}

impl DropReason {
    pub fn as_str(self) -> &'static str {
        match self {
            DropReason::UnknownMetric => "unknown_metric",
            DropReason::WrongLabels => "wrong_labels",
        }
    }
}

// ── Helpers ───────────────────────────────────────────────────────────────────

/// Validates the guest-supplied label key-value pairs against the declared schema
/// and returns an ordered value slice: `[app_name, app_namespace, ...declared_order...]`.
///
/// Returns `Err(())` if the guest does not supply exactly the declared keys.
fn build_label_values(
    app_name: &str,
    app_namespace: &str,
    declared_keys: &[String],
    guest_labels: &[(String, String)],
) -> Result<Vec<String>, ()> {
    if guest_labels.len() != declared_keys.len() {
        return Err(());
    }
    let guest_map: HashMap<&str, &str> = guest_labels
        .iter()
        .map(|(k, v)| (k.as_str(), v.as_str()))
        .collect();
    let mut values = Vec::with_capacity(2 + declared_keys.len());
    values.push(app_name.to_string());
    values.push(app_namespace.to_string());
    for key in declared_keys {
        match guest_map.get(key.as_str()) {
            Some(v) => values.push(v.to_string()),
            None => return Err(()),
        }
    }
    Ok(values)
}
