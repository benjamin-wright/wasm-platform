# monitoring

Local development Prometheus records execution-host preparation and dispatch health, gateway request ownership, and Linux process metrics. The root Tiltfile deploys it alongside the platform; the production platform chart does not deploy monitoring.

Open [Prometheus](http://localhost:9091) while Tilt is running. Use **Query** to graph an expression over a chosen historical range, and **Status → Target health** to inspect scrapes. [The configuration](helm/prometheus.yaml) scrapes each execution-host and gateway pod directly on its named metrics port, retaining `namespace`, `pod`, and `pod_uid` labels. Replicas have independent counters; a load-balanced Service is not a scrape target. Discovery has read-only access to pods in the deployment namespace.

## History

[The deployment](helm/templates/monitoring.yaml) retains seven days of samples or 1 GiB of retained storage, whichever removes older data first, using a 2 GiB persistent volume. Retention runs in the background; the write-ahead log, head chunks, and compaction need additional disk space. The size setting is not a hard total-disk limit. See [Prometheus storage](https://prometheus.io/docs/prometheus/latest/storage/) for those limits.

History survives Prometheus pod and Tilt process restarts while the PVC remains. Deleting the PVC or local cluster removes history. A local storage provisioner is required; the development k3d cluster provides one. This collector has one replica and no backup or remote storage.

Preparation, dispatch and process metrics are retained from hosts; gateway ownership and process metrics are retained from gateways. Prometheus also records its own health metrics. Scrape interval and metric selection are owned by [the scrape configuration](helm/prometheus.yaml); metric meanings are documented in [execution-host](../execution-host/README.md#metrics) and [gateway](../gateway/README.md#metrics).

## Tilt review resources

Trigger these manual resources in Tilt:

- `monitoring: config-check` validates the deployed scrape configuration with Prometheus's `promtool`.
- `monitoring: traffic` sends up to 32 requests per second to the demo application's `/hello` route for one minute, using eight workers and finite request timeouts. It prints response counts and exercises both HTTP and message invocations.
- `monitoring: review-history` reports active scrape targets and each pod's process start time, scrape failures, preparation and dispatch counts, reserved preparation capacity, configured limits, lifetime high-water marks, invariant counters, outcomes, and five-minute sampled work peaks. Preparation timings include lifetime means and observation counts. RSS minimum, maximum, latest value, sample count, and timestamps come from retained samples. Missing data is reported as unavailable.

The [review helper](review.py) uses only the Python standard library and bounds query duration, returned series, response bytes, and printed pods. Run traffic separately from e2e tests: both use the demo application's counters. Review history after traffic ends and another scrape completes to compare activity with idle occupancy and memory.

## Review queries

Paste these expressions into Prometheus and select a range containing the traffic or shutdown under review. Keep pod identities separate when comparing capacity and memory; aggregate rates only after calculating each counter's rate.

| Question | PromQL |
|---|---|
| How many gateway requests and connections are retained? | `wasm_gateway_work` |
| How close are gateway requests and connections to their limits? | `wasm_gateway_work / wasm_gateway_limit` |
| What was the gateway's highest observed request occupancy? | `wasm_gateway_high_water{pool="in_flight"}` |
| Are gateway requests being rejected or cancelled? | `increase(wasm_gateway_requests_rejected_total[1h])` and `increase(wasm_gateway_requests_cancelled_total[1h])` |
| Is any gateway failing scrapes? | `up{job="gateway"} == 0` |
| Does gateway resident memory settle after traffic stops? | `process_resident_memory_bytes{job="gateway"}` |
| How much module-loading work is waiting or active? | `wasm_host_preparation_work` |
| How much preparation capacity is currently reserved? | `wasm_host_preparation_occupancy / wasm_host_preparation_limit` |
| What was the sampled compilation backlog over the last hour? | `max_over_time(wasm_host_preparation_work[1h])` |
| How close has preparation come to its capacity limits? | `wasm_host_preparation_high_water / wasm_host_preparation_limit` |
| Have preparation capacity or accounting invariants failed? | `increase(wasm_host_preparation_capacity_breaches_total[1h])` and `increase(wasm_host_preparation_accounting_errors_total[1h])` |
| Were preparation submissions admitted, timed out or cancelled? | `increase(wasm_host_preparation_submissions_total[1h])` |
| How did admitted preparations and actual compiler workers finish? | `increase(wasm_host_preparation_finished_total[1h])` and `increase(wasm_host_preparation_compilation_finished_total[1h])` |
| What is the 99th percentile preparation admission wait? | `histogram_quantile(0.99, sum by (namespace, pod, pod_uid, le) (rate(wasm_host_preparation_admission_wait_seconds_bucket[5m])))` |
| What is the 99th percentile admitted queue wait? | `histogram_quantile(0.99, sum by (namespace, pod, pod_uid, le) (rate(wasm_host_preparation_queue_wait_seconds_bucket[5m])))` |
| What is the 99th percentile compiler lifetime? | `histogram_quantile(0.99, sum by (namespace, pod, pod_uid, le) (rate(wasm_host_preparation_compilation_seconds_bucket[5m])))` |
| How long does compilation continue after timeout or cancellation? | `histogram_quantile(0.99, sum by (namespace, pod, pod_uid, le) (rate(wasm_host_preparation_compilation_overrun_seconds_bucket[5m])))` |
| How much work is waiting, running, cleaning up, or awaiting collection? | `wasm_host_dispatch_work` |
| What were the largest observed work counts in the last hour? | `max_over_time(wasm_host_dispatch_work[1h])` |
| How close has each bounded pool come to its configured limit since process start? | `wasm_host_dispatch_high_water / wasm_host_dispatch_limit` |
| Have capacity or accounting invariants failed? | `increase(wasm_host_dispatch_capacity_breaches_total[1h])` and `increase(wasm_host_dispatch_accounting_errors_total[1h])` |
| Has dispatch admitted work after stopping? | `increase(wasm_host_dispatch_admissions_after_stop_total[1h])` |
| Are finished task records staying uncollected? | `min_over_time(wasm_host_dispatch_work{state="completed_uncollected"}[1m])` |
| What outcomes occurred? | `increase(wasm_host_dispatch_tasks_finished_total[1h])` |
| Were task crashes collected? | `increase(wasm_host_dispatch_tasks_reaped_total{outcome="panic"}[1h])` |
| Why did requests leave the waiting queue? | `increase(wasm_host_dispatch_queue_exits_total[1h])` |
| What is the 99th percentile queue wait? | `histogram_quantile(0.99, sum by (namespace, pod, pod_uid, le) (rate(wasm_host_dispatch_queue_wait_seconds_bucket[5m])))` |
| What is the 99th percentile execution lifetime? | `histogram_quantile(0.99, sum by (namespace, pod, pod_uid, le) (rate(wasm_host_dispatch_execution_seconds_bucket[5m])))` |
| How long does cancelled work take to exit? | `histogram_quantile(0.99, sum by (namespace, pod, pod_uid, le) (rate(wasm_host_dispatch_cancellation_to_exit_seconds_bucket[5m])))` |
| Is the host draining? | `wasm_host_dispatch_draining` |
| What drain durations were recorded? | `increase(wasm_host_dispatch_drain_seconds_sum[1h]) / increase(wasm_host_dispatch_drain_seconds_count[1h])` |
| Does resident memory settle after traffic stops? | `process_resident_memory_bytes{job="execution-host"}` |
| Is any host failing scrapes? | `up{job="execution-host"} == 0` |
| Which process does a host's lifetime count describe? | `process_start_time_seconds{job="execution-host"}` |
| How many series does the collector retain in memory? | `prometheus_tsdb_head_series{job="prometheus"}` |

Dispatch work states overlap: tracked tasks include active, replying, and completed tasks, so adding every state does not produce a total. Compare the execution high-water mark with its limit to review running plus terminating work. A brief nonzero completed count is normal; a count remaining positive after traffic ends deserves investigation. Resident memory includes runtime caches and allocator retention as well as task records, so compare memory with work counts and similar traffic periods.

Capacity breaches, accounting errors, and admissions after stop should remain zero. Historical occupancy, queue wait, cancellation latency, failure rates, and scrape availability provide baselines for operational thresholds. No alert rules or notification service are configured.

## Observation limits

Gauges are sampled and can miss events shorter than a scrape interval. Counters and high-water marks preserve brief events while a process remains alive, but reset when it restarts. `rate` and `increase` account for resets; estimates over small windows need multiple samples. Histogram quantiles are estimates and are undefined when no observations occurred.

A process may exit before its final counters or drain observation are scraped. An absent series is not a zero; check target health and historical scrape gaps before interpreting it. Pod UID separates replacement pods; `process_start_time_seconds` identifies container process restarts within a pod. Sampled peaks can span those restarts, while current high-water marks describe only the current process. Queue metrics cover host-owned waiting work, not buffers inside NATS clients or the broker. Process metrics are available in Linux deployments. History reveals trends and anomalies; it does not prove every cancellation or shutdown interleaving.
