//go:build integration

package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func TestMain(m *testing.M) {
	for _, app := range []string{"demo-app", "counter-app"} {
		cmd := exec.Command("kubectl", "wait", "application", app,
			"-n", "examples",
			"--for=condition=Ready",
			"--timeout=120s",
		)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "%s not ready: %v\n", app, err)
			os.Exit(1)
		}
	}
	os.Exit(m.Run())
}

const (
	baseURL       = "http://localhost/hello"
	counterAppURL = "http://localhost/counter"
	routeTimeout  = 60 * time.Second
	pollInterval  = 1 * time.Second
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
	g.Eventually(func() (counters, error) {
		c, err := fetch(baseURL)
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
	g.Eventually(func() (bool, error) {
		second, err := fetch(baseURL)
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
func fetch(url string) (counters, error) {
	resp, err := http.Get(url)
	if err != nil {
		return counters{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return counters{}, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return counters{}, fmt.Errorf("reading body: %w", err)
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
func fetchCounter(url string) (int, error) {
	resp, err := http.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("reading body: %w", err)
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
	g.Eventually(func() error {
		_, err := fetchCounter(counterAppURL)
		return err
	}, routeTimeout, pollInterval).Should(Succeed(),
		"counter-app should be serving at %s within %s", counterAppURL, routeTimeout)

	// ---- Part 1: hitting counter-app must not affect demo-app ----

	// Each call to fetch/fetchCounter is a GET that both increments and reads
	// the counter. To avoid off-by-one arithmetic, we capture the return value
	// of the last loop iteration as the "after" value rather than making a
	// separate post-loop verification call.

	demoBaseline, err := fetch(baseURL)
	g.Expect(err).NotTo(HaveOccurred())

	counterBaseline, err := fetchCounter(counterAppURL)
	g.Expect(err).NotTo(HaveOccurred())

	const n = 5
	var counterAfter int
	for i := 0; i < n; i++ {
		counterAfter, err = fetchCounter(counterAppURL)
		g.Expect(err).NotTo(HaveOccurred())
	}

	// counter-app should have advanced by exactly n.
	g.Expect(counterAfter).To(Equal(counterBaseline+n),
		"counter-app requests counter should have incremented by %d", n)

	// demo-app requests counter must be unchanged — the only call to demo-app
	// since the baseline was the demoAfter call itself.
	demoAfter, err := fetch(baseURL)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(demoAfter.requests).To(Equal(demoBaseline.requests+1),
		"demo-app KV store must be isolated from counter-app")

	// ---- Part 2: hitting demo-app must not affect counter-app ----

	// Use the last known counter-app value (counterAfter) as the new baseline.
	// This avoids an extra fetchCounter call that would itself increment the counter.
	counterBaseline2 := counterAfter

	var demoAfter2 counters
	for i := 0; i < n; i++ {
		demoAfter2, err = fetch(baseURL)
		g.Expect(err).NotTo(HaveOccurred())
	}

	// demo-app should have advanced by exactly n from the post-Part-1 baseline.
	g.Expect(demoAfter2.requests).To(Equal(demoAfter.requests+n),
		"demo-app requests counter should have incremented by %d", n)

	// counter-app must be unchanged — we haven't called it since counterAfter.
	// One call is needed to verify; it will itself increment by 1.
	counterAfter2, err := fetchCounter(counterAppURL)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(counterAfter2).To(Equal(counterBaseline2+1),
		"counter-app KV store must be isolated from demo-app")
}
