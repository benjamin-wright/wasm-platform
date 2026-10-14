"""Small, bounded traffic and recorded-history helpers for Tilt resources."""

import argparse
from collections import Counter
from concurrent.futures import ThreadPoolExecutor, wait
from datetime import datetime, timezone
import json
import math
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.parse import urlencode
from urllib.request import urlopen


MAX_RESPONSE_BYTES = 2 * 1024 * 1024
MAX_SERIES = 1024
MAX_PODS = 8
HISTORY_SECONDS = 300
DISPATCH_PREFIX = "wasm_host_dispatch_"
PREPARATION_PREFIX = "wasm_host_preparation_"
DISPATCH_STATES = ("queued", "tracked", "completed_uncollected", "running", "terminating")
PREPARATION_STATES = ("admission_wait", "queued", "preparing", "compiling", "terminating")
PREPARATION_KINDS = ("queued", "active", "tracked", "compiling")
PREPARATION_OUTCOMES = ("success", "error", "timeout", "cancelled", "panic")
PREPARATION_TIMINGS = (
    "admission_wait_seconds",
    "queue_wait_seconds",
    "compilation_seconds",
    "compilation_overrun_seconds",
)
METRICS = [
    DISPATCH_PREFIX + suffix
    for suffix in (
        "work",
        "limit",
        "high_water",
        "capacity_breaches_total",
        "accounting_errors_total",
        "admissions_after_stop_total",
        "tasks_finished_total",
        "tasks_reaped_total",
        "draining",
    )
] + [
    PREPARATION_PREFIX + suffix
    for suffix in (
        "work",
        "occupancy",
        "limit",
        "high_water",
        "capacity_breaches_total",
        "accounting_errors_total",
        "submissions_total",
        "finished_total",
        "compilation_finished_total",
    )
] + [
    PREPARATION_PREFIX + timing + suffix
    for timing in PREPARATION_TIMINGS
    for suffix in ("_sum", "_count")
] + ["process_resident_memory_bytes", "process_start_time_seconds", "up"]


def fetch(url, parameters):
    request_url = url + "?" + urlencode(parameters)
    with urlopen(request_url, timeout=5) as response:
        body = response.read(MAX_RESPONSE_BYTES + 1)
    if len(body) > MAX_RESPONSE_BYTES:
        raise ValueError("response exceeds the 2 MiB review limit")
    result = json.loads(body)
    if result.get("status") != "success":
        raise ValueError("Prometheus returned an unsuccessful response")
    for warning in result.get("warnings", [])[:3]:
        print("Prometheus warning:", str(warning)[:300], flush=True)
    return result["data"]


def pod_identity(labels):
    return tuple(labels.get(label, "unavailable") for label in ("namespace", "pod", "pod_uid"))


def numeric(value):
    number = float(value)
    return number if math.isfinite(number) else None


def display(value):
    return "unavailable" if value is None else f"{value:g}"


def timestamp(value):
    return datetime.fromtimestamp(value, timezone.utc).isoformat(timespec="seconds")


def grouped_series(series):
    grouped = {}
    for item in series:
        grouped.setdefault(pod_identity(item["metric"]), []).append(item)
    return grouped


def family_values(series, name, label):
    return {
        item["metric"].get(label, "total"): numeric(item["value"][1])
        for item in series
        if item["metric"]["__name__"] == name
    }


def print_family(series, prefix, suffix, label, expected):
    values = family_values(series, prefix + suffix, label)
    fields = ", ".join(f"{key}={display(values.get(key))}" for key in expected)
    print(f"  {suffix}: {fields}")


def family_total(series, prefix, suffix):
    values = family_values(series, prefix + suffix, "outcome")
    if not values or any(value is None for value in values.values()):
        return None
    return sum(values.values())


def history_values(series, name, state=None):
    samples = []
    for item in series:
        if item["metric"]["__name__"] != name:
            continue
        if state is not None and item["metric"].get("state") != state:
            continue
        for sampled_at, value in item["values"]:
            number = numeric(value)
            if number is not None:
                samples.append((sampled_at, number))
    return sorted(samples)


def print_work_review(snapshot, samples, prefix, states, kinds):
    print_family(snapshot, prefix, "work", "state", states)
    for suffix in ("limit", "high_water", "capacity_breaches_total"):
        print_family(snapshot, prefix, suffix, "kind", kinds)
    print_family(snapshot, prefix, "accounting_errors_total", "state", states)
    peaks = []
    for state in states:
        values = history_values(samples, prefix + "work", state)
        peaks.append(f"{state}={display(max((value for _, value in values), default=None))}")
    print("  five-minute sampled work peaks: " + ", ".join(peaks))


def print_timing(series, prefix, suffix):
    total = family_values(series, prefix + suffix + "_sum", "").get("total")
    count = family_values(series, prefix + suffix + "_count", "").get("total")
    mean = total / count if total is not None and count is not None and count > 0 else None
    print(f"  {suffix}: lifetime_mean={display(mean)}, observations={display(count)}")


def review(base_url):
    end = time.time()
    selector = '{job="execution-host",__name__=~"' + "|".join(METRICS) + '"}'
    targets = fetch(base_url + "/api/v1/targets", {"state": "active", "scrapePool": "execution-host"})
    active = targets.get("activeTargets", [])
    print(f"Reviewing retained samples for {timestamp(end - HISTORY_SECONDS)} to {timestamp(end)}")
    print(f"Active execution-host targets: {len(active)}")
    for target in active[:MAX_PODS]:
        identity = "/".join(pod_identity(target["labels"]))
        error = "; last scrape failed" if target.get("lastError") else ""
        print(f"  {identity}: health={target['health']}, last_scrape={target['lastScrape']}{error}")
    if not active:
        print("  Target discovery is unavailable; absence is not a healthy zero.")
    parameters = {"time": end, "timeout": "3s", "limit": MAX_SERIES}
    current = fetch(base_url + "/api/v1/query", {**parameters, "query": selector})
    history = fetch(base_url + "/api/v1/query", {**parameters, "query": selector + "[5m]"})
    if current["resultType"] != "vector" or history["resultType"] != "matrix":
        raise ValueError("unexpected Prometheus query result type")
    if max(len(current["result"]), len(history["result"])) >= MAX_SERIES:
        print("Series limit reached; this review may be incomplete.")
    snapshots = grouped_series(current["result"])
    retained = grouped_series(history["result"])
    identities = sorted(set(snapshots) | set(retained))
    if not identities:
        print("Host work and process data unavailable; no recorded series were returned.")
    if len(identities) > MAX_PODS or len(active) > MAX_PODS:
        print(f"Output limited to {MAX_PODS} pods; inspect other pods in Prometheus.")
    for identity in identities[:MAX_PODS]:
        snapshot = snapshots.get(identity, [])
        samples = retained.get(identity, [])
        print("Pod " + "/".join(identity))
        started = family_values(snapshot, "process_start_time_seconds", "").get("total")
        starts = {value for _, value in history_values(samples, "process_start_time_seconds")}
        print(f"  process_started={timestamp(started) if started is not None else 'unavailable'}, observed_processes={len(starts) if starts else 'unavailable'}")
        up = family_values(snapshot, "up", "").get("total")
        scrapes = history_values(samples, "up")
        failed = sum(value == 0 for _, value in scrapes) if scrapes else None
        print(f"  scrape_up={display(up)}, five-minute failed scrape samples={display(failed)}")
        print(" Dispatch")
        print_work_review(snapshot, samples, DISPATCH_PREFIX, DISPATCH_STATES, ("queued", "execution", "tracked"))
        print_family(snapshot, DISPATCH_PREFIX, "admissions_after_stop_total", "", ("total",))
        print(f"  task totals since process start: finished={display(family_total(snapshot, DISPATCH_PREFIX, 'tasks_finished_total'))}, reaped={display(family_total(snapshot, DISPATCH_PREFIX, 'tasks_reaped_total'))}")
        print(" Preparation")
        print_work_review(snapshot, samples, PREPARATION_PREFIX, PREPARATION_STATES, PREPARATION_KINDS)
        print_family(snapshot, PREPARATION_PREFIX, "occupancy", "kind", PREPARATION_KINDS)
        print_family(snapshot, PREPARATION_PREFIX, "submissions_total", "outcome", ("admitted", "timeout", "cancelled", "closed"))
        for suffix in ("finished_total", "compilation_finished_total"):
            print_family(snapshot, PREPARATION_PREFIX, suffix, "outcome", PREPARATION_OUTCOMES)
        for suffix in PREPARATION_TIMINGS:
            print_timing(snapshot, PREPARATION_PREFIX, suffix)
        rss = history_values(samples, "process_resident_memory_bytes")
        if rss:
            latest = numeric(next((item["value"][1] for item in snapshot if item["metric"]["__name__"] == "process_resident_memory_bytes"), "nan"))
            print(f"  RSS bytes: min={min(value for _, value in rss):g}, max={max(value for _, value in rss):g}, latest={display(latest)}, retained_samples={len(rss)}, first={timestamp(rss[0][0])}, last={timestamp(rss[-1][0])}")
        else:
            print("  RSS history unavailable; retained_samples=unavailable")
    print("Counts are observations, not assertions. Short events and final shutdown samples may be missed.")


def traffic(url, seconds, workers):
    deadline = time.monotonic() + seconds
    statuses = Counter()
    lock = threading.Lock()

    def worker():
        while (remaining := deadline - time.monotonic()) > 0:
            started = time.monotonic()
            try:
                with urlopen(url, timeout=min(2, remaining)) as response:
                    body = response.read(65537)
                    status = str(response.status) if len(body) <= 65536 else "oversized_response"
            except HTTPError as error:
                status = str(error.code)
                error.close()
            except (URLError, TimeoutError, OSError):
                status = "request_error"
            with lock:
                statuses[status] += 1
            time.sleep(max(0, min(deadline - time.monotonic(), 0.25 - (time.monotonic() - started))))

    print(f"Sending GET requests to {url} for {seconds}s with {workers} workers; maximum {workers * 4} requests/s", flush=True)
    with ThreadPoolExecutor(max_workers=workers) as pool:
        pending = {pool.submit(worker) for _ in range(workers)}
        while pending:
            finished, pending = wait(pending, timeout=15)
            for future in finished:
                future.result()
            with lock:
                print("Traffic statuses:", dict(sorted(statuses.items())), flush=True)
    print("Traffic finished. Review historical work and RSS after the collector's next scrape.", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    review_parser = commands.add_parser("review")
    review_parser.add_argument("--prometheus-url", default="http://localhost:9091")
    traffic_parser = commands.add_parser("traffic")
    traffic_parser.add_argument("--url", default="http://localhost/hello")
    traffic_parser.add_argument("--seconds", type=int, choices=range(1, 121), default=60)
    traffic_parser.add_argument("--workers", type=int, choices=range(1, 17), default=8)
    arguments = parser.parse_args()
    try:
        if arguments.command == "review":
            review(arguments.prometheus_url.rstrip("/"))
        else:
            traffic(arguments.url, arguments.seconds, arguments.workers)
    except (URLError, TimeoutError, OSError, ValueError, KeyError) as error:
        parser.exit(1, f"Monitoring review unavailable: {type(error).__name__}: {str(error)[:300]}\n")


if __name__ == "__main__":
    main()
