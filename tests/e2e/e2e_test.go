//go:build integration

package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func TestMain(m *testing.M) {
	type appRef struct {
		name, namespace string
	}
	for _, app := range []appRef{
		{"demo-app", "examples"},
		{"counter-app", "examples"},
		{"sql-hello", "default"},
	} {
		out, err := kubectlOutput(context.Background(), 125*time.Second, nil, true,
			"wait", "application", app.name,
			"-n", app.namespace,
			"--for=condition=Ready",
			"--timeout=120s",
		)
		fmt.Fprint(os.Stdout, string(out))
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s not ready: %v\n", app.name, err)
			os.Exit(1)
		}
	}
	code := m.Run()
	testHTTPClient.CloseIdleConnections()
	os.Exit(code)
}

const (
	baseURL           = "http://localhost/hello"
	counterAppURL     = "http://localhost/counter"
	sqlHelloURL       = "http://localhost/sql-hello"
	sqlHelloSetupURL  = "http://localhost/sql-hello/setup"
	sqlHelloInsertURL = "http://localhost/sql-hello/insert"
	sqlBrokenURL      = "http://localhost/sql-broken"
	routeTimeout      = 60 * time.Second
	pollInterval      = 1 * time.Second
)

type counters struct {
	requests int
	messages int
}

func TestHelloWorldEndToEnd(t *testing.T) {
	g := NewWithT(t)

	// Poll until the full stack (operator → module-cache → execution-host →
	// gateway) is serving. Capture the first successful response so we can
	// assert counter increments against it without a separate bare fetch that
	// could race during startup.
	var first counters
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (counters, error) {
		c, err := fetch(ctx, baseURL)
		if err != nil {
			return counters{}, err
		}
		first = c
		return c, nil
	}, routeTimeout, pollInterval).Should(Not(Equal(counters{})),
		"hello-world module should be serving at %s within %s", baseURL, routeTimeout)

	// The messages counter is incremented asynchronously by the message-counter
	// module after each request. Poll until both requests and messages have
	// advanced beyond the first call's values.
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (bool, error) {
		second, err := fetch(ctx, baseURL)
		if err != nil {
			return false, err
		}
		return second.requests > first.requests && second.messages > first.messages, nil
	}, routeTimeout, pollInterval).Should(BeTrue(),
		"expected both requests and messages counters to increment: first=%+v", first)
}

// fetch makes a GET request and returns the parsed counters from the
// hello-world response body. Returns an error if the request fails, the
// status is not 200, or the body does not contain the expected counters.
func fetch(ctx context.Context, url string) (counters, error) {
	status, bodyBytes, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, url, "", nil)
	if err != nil {
		return counters{}, err
	}
	if status != http.StatusOK {
		return counters{}, fmt.Errorf("unexpected status %d", status)
	}
	return parseCounters(string(bodyBytes))
}

// parseCounters extracts requests=N and messages=M from the response body.
func parseCounters(body string) (counters, error) {
	var c counters
	var gotRequests, gotMessages bool
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "requests=") {
			n, err := strconv.Atoi(strings.TrimPrefix(field, "requests="))
			if err != nil {
				return counters{}, fmt.Errorf("parsing requests field: %w", err)
			}
			c.requests = n
			gotRequests = true
		}
		if strings.HasPrefix(field, "messages=") {
			n, err := strconv.Atoi(strings.TrimPrefix(field, "messages="))
			if err != nil {
				return counters{}, fmt.Errorf("parsing messages field: %w", err)
			}
			c.messages = n
			gotMessages = true
		}
	}
	if !gotRequests {
		return counters{}, fmt.Errorf("no requests= field found in %q", body)
	}
	if !gotMessages {
		return counters{}, fmt.Errorf("no messages= field found in %q", body)
	}
	return c, nil
}

// fetchCounter makes a GET request to counter-app and returns the parsed
// requests counter. Returns an error if the request fails, the status is not
// 200, or the body does not contain the expected field.
func fetchCounter(ctx context.Context, url string) (int, error) {
	status, bodyBytes, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, url, "", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d", status)
	}
	body := string(bodyBytes)
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "requests=") {
			n, err := strconv.Atoi(strings.TrimPrefix(field, "requests="))
			if err != nil {
				return 0, fmt.Errorf("parsing requests field: %w", err)
			}
			return n, nil
		}
	}
	return 0, fmt.Errorf("no requests= field found in %q", body)
}

// TestKVIsolation verifies that counter-app and demo-app maintain independent
// KV stores even though they use identical store and key names.
func TestKVIsolation(t *testing.T) {
	g := NewWithT(t)

	// Wait for counter-app to be serving before taking any baseline.
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) error {
		_, err := fetchCounter(ctx, counterAppURL)
		return err
	}, routeTimeout, pollInterval).Should(Succeed(),
		"counter-app should be serving at %s within %s", counterAppURL, routeTimeout)

	// ---- Part 1: hitting counter-app must not affect demo-app ----

	// Each call to fetch/fetchCounter is a GET that both increments and reads
	// the counter. To avoid off-by-one arithmetic, we capture the return value
	// of the last loop iteration as the "after" value rather than making a
	// separate post-loop verification call.

	demoBaseline, err := fetch(t.Context(), baseURL)
	g.Expect(err).NotTo(HaveOccurred())

	counterBaseline, err := fetchCounter(t.Context(), counterAppURL)
	g.Expect(err).NotTo(HaveOccurred())

	const n = 5
	var counterAfter int
	for i := 0; i < n; i++ {
		counterAfter, err = fetchCounter(t.Context(), counterAppURL)
		g.Expect(err).NotTo(HaveOccurred())
	}

	// counter-app should have advanced by exactly n.
	g.Expect(counterAfter).To(Equal(counterBaseline+n),
		"counter-app requests counter should have incremented by %d", n)

	// demo-app requests counter must be unchanged — the only call to demo-app
	// since the baseline was the demoAfter call itself.
	demoAfter, err := fetch(t.Context(), baseURL)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(demoAfter.requests).To(Equal(demoBaseline.requests+1),
		"demo-app KV store must be isolated from counter-app")

	// ---- Part 2: hitting demo-app must not affect counter-app ----

	// Use the last known counter-app value (counterAfter) as the new baseline.
	// This avoids an extra fetchCounter call that would itself increment the counter.
	counterBaseline2 := counterAfter

	var demoAfter2 counters
	for i := 0; i < n; i++ {
		demoAfter2, err = fetch(t.Context(), baseURL)
		g.Expect(err).NotTo(HaveOccurred())
	}

	// demo-app should have advanced by exactly n from the post-Part-1 baseline.
	g.Expect(demoAfter2.requests).To(Equal(demoAfter.requests+n),
		"demo-app requests counter should have incremented by %d", n)

	// counter-app must be unchanged — we haven't called it since counterAfter.
	// One call is needed to verify; it will itself increment by 1.
	counterAfter2, err := fetchCounter(t.Context(), counterAppURL)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(counterAfter2).To(Equal(counterBaseline2+1),
		"counter-app KV store must be isolated from demo-app")
}

const metricsURL = "http://localhost:9090/metrics"

// TestMetrics invokes the demo-app endpoint then scrapes the execution-host
// /metrics endpoint and asserts that both the user-defined counter
// (demo_requests_total) and the platform counter (wasm_host_events_received_total)
// are non-zero.
func TestMetrics(t *testing.T) {
	g := NewWithT(t)

	// Trigger at least one request so both counters advance.
	_, err := fetch(t.Context(), baseURL)
	g.Expect(err).NotTo(HaveOccurred())

	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (float64, error) {
		return scrapeMetricSum(ctx, metricsURL, "demo_requests_total")
	}, routeTimeout, pollInterval).Should(BeNumerically(">", 0),
		"demo_requests_total should be non-zero after invoking demo-app")

	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (float64, error) {
		return scrapeMetricSum(ctx, metricsURL, "wasm_host_events_received_total")
	}, routeTimeout, pollInterval).Should(BeNumerically(">", 0),
		"wasm_host_events_received_total should be non-zero after invoking demo-app")

	for _, name := range []string{
		"wasm_host_dispatch_limit",
		"wasm_host_dispatch_tasks_finished_total",
		"wasm_host_dispatch_tasks_reaped_total",
		"process_resident_memory_bytes",
	} {
		g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (float64, error) {
			return scrapeMetricSum(ctx, metricsURL, name)
		}, routeTimeout, pollInterval).Should(BeNumerically(">", 0), name+" should be exposed after an invocation")
	}
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (float64, error) {
		return scrapeMetricSum(ctx, metricsURL, "wasm_host_dispatch_work")
	}, routeTimeout, pollInterval).Should(Equal(float64(0)),
		"dispatch work should return to zero after traffic finishes")
}

// scrapeMetricSum fetches the Prometheus text output at url and returns the sum
// of all sample values whose metric base name equals name.
func scrapeMetricSum(ctx context.Context, url, name string) (float64, error) {
	status, bodyBytes, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, url, "", nil)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d", status)
	}
	return metricSum(string(bodyBytes), name)
}

// TestSQLHello exercises the sql-hello Application using its three HTTP
// handler functions:
//
//  1. POST /sql-hello/setup  — seeds the greetings table (writer user).
//  2. GET  /sql-hello        — queries active rows (reader user); asserts
//     Alice and Bob are present and Carol is absent.
//  3. POST /sql-hello/insert — attempts an INSERT with the reader user;
//     expects HTTP 403 (permission denied enforced by PostgreSQL).
//
// No external seed Job is required. TestMain already waits for the
// sql-hello Application to reach Ready before tests run.
func TestSQLHello(t *testing.T) {
	g := NewWithT(t)

	// Step 1: seed the table via the setup handler (writer user).
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) error {
		status, _, err := requestHTTP(ctx, testHTTPClient, http.MethodPost, sqlHelloSetupURL, "", nil)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("setup returned %d", status)
		}
		return nil
	}, routeTimeout, pollInterval).Should(Succeed(),
		"POST %s should return 200 within %s", sqlHelloSetupURL, routeTimeout)

	// Step 2: query active rows; assert Alice and Bob present, Carol absent.
	var body string
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (string, error) {
		status, b, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, sqlHelloURL, "", nil)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("query returned status %d", status)
		}
		s := string(b)
		if s == "" || s == "[]" {
			return "", fmt.Errorf("empty response body")
		}
		body = s
		return s, nil
	}, routeTimeout, pollInterval).ShouldNot(BeEmpty(),
		"GET %s should return a non-empty JSON response within %s", sqlHelloURL, routeTimeout)

	g.Expect(body).To(ContainSubstring(`"name":"Alice"`),
		"expected Alice (active=true) in sql-hello response")
	g.Expect(body).To(ContainSubstring(`"name":"Bob"`),
		"expected Bob (active=true) in sql-hello response")
	g.Expect(body).NotTo(ContainSubstring(`"name":"Carol"`),
		"expected Carol (active=false) to be excluded from sql-hello response")

	// Step 3: reader user attempts INSERT — must be rejected with 403.
	status, _, err := requestHTTP(t.Context(), testHTTPClient, http.MethodPost, sqlHelloInsertURL, "", nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(status).To(Equal(http.StatusForbidden),
		"reader user INSERT should be rejected with 403 (permission denied)")
}

// migrationFailedTimeout bounds how long we'll wait for the db-operator to run
// the broken migration, fail it, and propagate the failure up to the
// Application status. It is deliberately longer than routeTimeout because the
// path involves CR creation, image pull, Job execution, and a status round-trip.
const migrationFailedTimeout = 180 * time.Second

// TestSQLBrokenMigrationsFailurePath verifies the migration failure path:
// an Application whose spec.sql.migrations references a deliberately broken
// migration (SELECT * FROM nonexistent;) must reach Ready=False with reason
// MigrationFailed, and the wp-operator must withhold function activation so
// no traffic is served on the declared HTTP route.
//
// The fixture (examples/sql-broken-migrations/) is intentionally not waited on
// in TestMain — its Application is expected never to become Ready.
func TestSQLBrokenMigrationsFailurePath(t *testing.T) {
	g := NewWithT(t)

	g.Eventually(pollContext(t, migrationFailedTimeout), func(ctx context.Context) (string, error) {
		return readyConditionReason(ctx, "sql-broken-migrations", "default")
	}, migrationFailedTimeout, pollInterval).Should(Equal("MigrationFailed"),
		"sql-broken-migrations Application should reach Ready=False, reason=MigrationFailed")

	// Sanity check: the gateway must not have wired up the function's route,
	// since wp-operator never pushed config. A consistent absence is what we
	// want, so we use Consistently rather than a single fetch.
	g.Consistently(pollContext(t, 5*time.Second+requestTimeout), func(ctx context.Context) (int, error) {
		status, _, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, sqlBrokenURL, "", nil)
		return status, err
	}, 5*time.Second, pollInterval).ShouldNot(Equal(http.StatusOK),
		"GET %s must not be served while the Application is in MigrationFailed", sqlBrokenURL)
}

// readyConditionReason shells out to kubectl to read the current Ready
// condition reason for an Application. Returns an empty string (not an error)
// when the condition is not yet present so that callers using Eventually keep
// polling rather than failing instead.
func readyConditionReason(ctx context.Context, name, namespace string) (string, error) {
	out, err := kubectlOutput(ctx, kubectlTimeout, nil, false, "get", "application", name,
		"-n", namespace,
		"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].reason}`,
	)
	if err != nil {
		return "", fmt.Errorf("kubectl get application %s/%s: %w", namespace, name, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// kubectlApply attempts to apply the given YAML via kubectl and returns the
// combined stdout+stderr output and any error.
func kubectlApply(ctx context.Context, yaml string) (string, error) {
	out, err := kubectlOutput(ctx, kubectlTimeout, strings.NewReader(yaml), true, "apply", "-f", "-")
	return string(out), err
}

// kubectlDelete deletes the resource described by the YAML, ignoring not-found errors.
func kubectlDelete(ctx context.Context, yaml string) error {
	_, err := kubectlOutput(ctx, kubectlTimeout, strings.NewReader(yaml), true,
		"delete", "--ignore-not-found", "--timeout=15s", "-f", "-")
	return err
}

func prepareTestApplications(t *testing.T, manifests ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, manifest := range manifests {
			if err := kubectlDelete(context.Background(), manifest); err != nil {
				t.Errorf("test Application cleanup failed: %v", err)
			}
		}
	})
	for _, manifest := range manifests {
		if err := kubectlDelete(t.Context(), manifest); err != nil {
			t.Fatalf("test Application preparation failed: %v", err)
		}
	}
}

// TestWebhookTopicConflictRejection verifies that applying an Application whose
// topic is already claimed by another Application is rejected at admission by
// the ValidatingWebhook, not silently accepted and surfaced via status.
func TestWebhookTopicConflictRejection(t *testing.T) {
	g := NewWithT(t)

	const owner = `
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: webhook-topic-owner
  namespace: default
spec:
  functions:
    - name: handler
      module: oci://wasm-platform-registry.localhost:5001/demo-app-http@sha256:0000000000000000000000000000000000000000000000000000000000000000
      trigger:
        topic: webhook.conflict.test
`
	const conflicting = `
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: webhook-topic-conflicting
  namespace: default
spec:
  functions:
    - name: handler
      module: oci://wasm-platform-registry.localhost:5001/demo-app-http@sha256:0000000000000000000000000000000000000000000000000000000000000000
      trigger:
        topic: webhook.conflict.test
`

	prepareTestApplications(t, owner, conflicting)

	_, err := kubectlApply(t.Context(), owner)
	g.Expect(err).NotTo(HaveOccurred(), "owner Application should be admitted")

	out, err := kubectlApply(t.Context(), conflicting)
	g.Expect(err).To(HaveOccurred(), "conflicting Application should be rejected at admission")
	g.Expect(out).To(ContainSubstring("webhook.conflict.test"), "rejection message should name the conflicting topic")
}

// TestWebhookMetricConflictRejection verifies that applying an Application whose
// metric name is already claimed by another Application is rejected at admission.
func TestWebhookMetricConflictRejection(t *testing.T) {
	g := NewWithT(t)

	const owner = `
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: webhook-metric-owner
  namespace: default
spec:
  functions:
    - name: handler
      module: oci://wasm-platform-registry.localhost:5001/demo-app-http@sha256:0000000000000000000000000000000000000000000000000000000000000000
      trigger:
        topic: webhook.metric.owner.events
  metrics:
    - name: webhook_conflict_metric_total
      type: counter
`
	const conflicting = `
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: webhook-metric-conflicting
  namespace: default
spec:
  functions:
    - name: handler
      module: oci://wasm-platform-registry.localhost:5001/demo-app-http@sha256:0000000000000000000000000000000000000000000000000000000000000000
      trigger:
        topic: webhook.metric.conflicting.events
  metrics:
    - name: webhook_conflict_metric_total
      type: counter
`

	prepareTestApplications(t, owner, conflicting)

	_, err := kubectlApply(t.Context(), owner)
	g.Expect(err).NotTo(HaveOccurred(), "owner Application should be admitted")

	out, err := kubectlApply(t.Context(), conflicting)
	g.Expect(err).To(HaveOccurred(), "conflicting Application should be rejected at admission")
	g.Expect(out).To(ContainSubstring("webhook_conflict_metric_total"), "rejection message should name the conflicting metric")
}

// TestWebhookInvalidIdentifierRejection verifies that applying an Application
// with a name containing consecutive hyphens ("--") and spec.sql set is
// rejected at admission.
func TestWebhookInvalidIdentifierRejection(t *testing.T) {
	g := NewWithT(t)

	const invalid = `
apiVersion: wasm-platform.io/v1alpha1
kind: Application
metadata:
  name: bad--name
  namespace: default
spec:
  sql: {}
  functions:
    - name: handler
      module: oci://wasm-platform-registry.localhost:5001/demo-app-http@sha256:0000000000000000000000000000000000000000000000000000000000000000
      trigger:
        topic: webhook.invalid.id.events
`

	prepareTestApplications(t, invalid)

	out, err := kubectlApply(t.Context(), invalid)
	g.Expect(err).To(HaveOccurred(), "Application with invalid identifier should be rejected at admission")
	g.Expect(out).To(ContainSubstring("consecutive hyphens"), "rejection message should describe the identifier constraint")
}
