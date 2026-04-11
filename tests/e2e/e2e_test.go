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
	cmd := exec.Command("kubectl", "wait", "application", "demo-app",
		"-n", "examples",
		"--for=condition=Ready",
		"--timeout=120s",
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "demo-app not ready: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

const (
	baseURL      = "http://localhost/hello"
	routeTimeout = 60 * time.Second
	pollInterval = 1 * time.Second
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
