use std::{collections::HashMap, sync::Arc};

use anyhow::Result;
use prometheus::{
    Encoder, Registry, TextEncoder,
    core::{Collector, Desc},
    proto::{Counter, Gauge, LabelPair, Metric, MetricFamily, MetricType},
};

use crate::supervisor::RequestSupervisor;

/// Prometheus reporting derived from gateway-owned request and connection state.
#[derive(Clone)]
pub struct MetricsRegistry {
    registry: Registry,
}

impl MetricsRegistry {
    pub fn new(supervisor: Arc<RequestSupervisor>) -> Result<Self> {
        let registry = Registry::new();
        registry.register(Box::new(RequestCollector::new(supervisor)?))?;
        #[cfg(target_os = "linux")]
        registry.register(Box::new(
            prometheus::process_collector::ProcessCollector::for_self(),
        ))?;
        Ok(Self { registry })
    }

    pub fn render(&self) -> Result<String> {
        let mut buffer = Vec::new();
        TextEncoder::new().encode(&self.registry.gather(), &mut buffer)?;
        Ok(String::from_utf8(buffer)?)
    }
}

struct RequestCollector {
    supervisor: Arc<RequestSupervisor>,
    work: Desc,
    limit: Desc,
    high_water: Desc,
    rejected: Desc,
    completed: Desc,
    cancelled: Desc,
}

impl RequestCollector {
    fn new(supervisor: Arc<RequestSupervisor>) -> Result<Self> {
        Ok(Self {
            supervisor,
            work: descriptor(
                "wasm_gateway_work",
                "Current gateway-owned requests and accepted connections",
                &["pool"],
            )?,
            limit: descriptor(
                "wasm_gateway_limit",
                "Configured gateway request and connection capacity",
                &["pool"],
            )?,
            high_water: descriptor(
                "wasm_gateway_high_water",
                "Highest observed in-flight request count since process startup",
                &["pool"],
            )?,
            rejected: descriptor(
                "wasm_gateway_requests_rejected_total",
                "Valid-route requests rejected for lack of gateway request capacity",
                &[],
            )?,
            completed: descriptor(
                "wasm_gateway_requests_completed_total",
                "Gateway-owned requests whose handler completed",
                &[],
            )?,
            cancelled: descriptor(
                "wasm_gateway_requests_cancelled_total",
                "Gateway-owned requests cancelled before their handler completed",
                &[],
            )?,
        })
    }
}

impl Collector for RequestCollector {
    fn desc(&self) -> Vec<&Desc> {
        vec![
            &self.work,
            &self.limit,
            &self.high_water,
            &self.rejected,
            &self.completed,
            &self.cancelled,
        ]
    }

    fn collect(&self) -> Vec<MetricFamily> {
        let snapshot = self.supervisor.snapshot();
        vec![
            gauges(
                &self.work,
                &[
                    ("in_flight", snapshot.in_flight),
                    ("connections", snapshot.connection_count),
                ],
            ),
            gauges(
                &self.limit,
                &[
                    ("in_flight", snapshot.in_flight_limit),
                    ("connections", snapshot.connection_limit),
                ],
            ),
            gauges(&self.high_water, &[("in_flight", snapshot.high_water)]),
            counter(&self.rejected, snapshot.rejected),
            counter(&self.completed, snapshot.completed),
            counter(&self.cancelled, snapshot.cancelled),
        ]
    }
}

fn descriptor(name: &str, help: &str, labels: &[&str]) -> Result<Desc> {
    Ok(Desc::new(
        name.to_owned(),
        help.to_owned(),
        labels.iter().map(|label| (*label).to_owned()).collect(),
        HashMap::new(),
    )?)
}

fn gauges(desc: &Desc, values: &[(&str, usize)]) -> MetricFamily {
    let metrics = values
        .iter()
        .map(|(pool, value)| {
            let mut label = LabelPair::default();
            label.set_name("pool".to_owned());
            label.set_value((*pool).to_owned());
            let mut gauge = Gauge::default();
            gauge.set_value(*value as f64);
            let mut metric = Metric::default();
            metric.set_label(vec![label]);
            metric.set_gauge(gauge);
            metric
        })
        .collect();
    family(desc, MetricType::GAUGE, metrics)
}

fn counter(desc: &Desc, value: u64) -> MetricFamily {
    let mut counter = Counter::default();
    counter.set_value(value as f64);
    let mut metric = Metric::default();
    metric.set_counter(counter);
    family(desc, MetricType::COUNTER, vec![metric])
}

fn family(desc: &Desc, metric_type: MetricType, metrics: Vec<Metric>) -> MetricFamily {
    let mut family = MetricFamily::default();
    family.set_name(desc.fq_name.clone());
    family.set_help(desc.help.clone());
    family.set_field_type(metric_type);
    family.set_metric(metrics);
    family
}
