//go:build integration

package e2e_test

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

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

	// Wait until the hello-world WASM module is fully serving — status 200
	// AND a valid "requests=N" counter in the body. This ensures the full
	// stack (operator → module-cache → execution-host → gateway) is ready
	// before we assert on counter increments.
	g.Eventually(func() error {
		_, err := fetch(baseURL)
		return err
	}, routeTimeout, pollInterval).Should(Succeed(),
		"hello-world module should be serving at %s within %s", baseURL, routeTimeout)

	first, err := fetch(baseURL)
	g.Expect(err).NotTo(HaveOccurred())

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

