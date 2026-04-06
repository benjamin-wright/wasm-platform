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

	second, err := fetch(baseURL)
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(second).To(BeNumerically(">", first),
		"expected request counter to increment: first=%d second=%d", first, second)
}

// fetch makes a GET request and returns the "requests=N" counter from the
// hello-world response body. Returns an error if the request fails, the
// status is not 200, or the body does not contain a counter.
func fetch(url string) (int, error) {
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
	return parseRequests(string(bodyBytes))
}

// parseRequests extracts the N from "requests=N" in the response body.
func parseRequests(body string) (int, error) {
	for _, field := range strings.Fields(body) {
		if strings.HasPrefix(field, "requests=") {
			return strconv.Atoi(strings.TrimPrefix(field, "requests="))
		}
	}
	return 0, fmt.Errorf("no requests= field found in %q", body)
}
