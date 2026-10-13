//go:build integration

package e2e_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	gatewayAddress    = "localhost:3000"
	gatewayMetricsURL = "http://localhost:9092"
)

// The production HTTP port is forwarded by Tilt. Raw requests preserve partial
// uploads and informational responses that an HTTP proxy may otherwise buffer.
func TestGatewayRequestOwnershipEndToEnd(t *testing.T) {
	limits := gatewayDeployedLimits(t)
	rejections := []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/gateway-unknown-route", http.StatusNotFound},
		{http.MethodPost, "/counter", http.StatusMethodNotAllowed},
	}
	t.Run("ingress-route-method-rejection", func(t *testing.T) {
		baseline := gatewayCounter(t)
		for _, test := range rejections {
			status, headers, _, err := requestHTTPWithHeaders(t.Context(), testHTTPClient,
				test.method, "http://localhost"+test.path, "small completed body", nil)
			if err != nil || status != test.status {
				t.Fatalf("ingress %s %s: HTTP %d, error %v; want %d", test.method, test.path, status, err, test.status)
			}
			if test.status == http.StatusMethodNotAllowed && headers.Get("Allow") != http.MethodGet {
				t.Errorf("Allow = %q; want GET", headers.Get("Allow"))
			}
		}
		gatewayAssertCounter(t, baseline+1)
	})
	t.Run("reject-before-body", func(t *testing.T) {
		baseline := gatewayCounter(t)
		for _, test := range rejections {
			conn, reader := gatewayOpenRequest(t, gatewayAddress, test.method, test.path,
				fmt.Sprintf("Content-Length: %d\r\nExpect: 100-continue\r\n", limits.bodyBytes+1), requestTimeout)
			response := gatewayReadResponse(t, reader)
			if response.StatusCode != test.status {
				t.Fatalf("%s %s returned %d before body upload; want %d", test.method, test.path, response.StatusCode, test.status)
			}
			if test.status == http.StatusMethodNotAllowed && response.Header.Get("Allow") != http.MethodGet {
				t.Errorf("Allow = %q; want GET", response.Header.Get("Allow"))
			}
			response.Body.Close()
			conn.Close()
		}
		gatewayAssertCounter(t, baseline+1)
	})

	t.Run("absolute-header-deadline", func(t *testing.T) {
		baseline := gatewayCounter(t)
		started := time.Now()
		conn, reader := gatewayConnect(t, gatewayAddress, limits.bodyTimeout+requestTimeout)
		if _, err := io.WriteString(conn, "GET /counter HTTP/1.1\r\nHost: localhost\r\nX-Slow: "); err != nil {
			t.Fatalf("sending partial request headers: %v", err)
		}
		stop := gatewayTrickle(t, conn, limits.bodyTimeout, "x")
		defer stop()
		response, err := http.ReadResponse(reader, nil)
		if err == nil {
			response.Body.Close()
			if response.StatusCode != http.StatusRequestTimeout {
				t.Fatalf("incomplete headers returned HTTP %d; want 408 or a closed connection", response.StatusCode)
			}
		} else if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, syscall.ECONNRESET) {
			t.Fatalf("gateway did not close the incomplete header stream within its deadline: %v", err)
		}
		stop()
		if elapsed := time.Since(started); elapsed < limits.bodyTimeout/2 || elapsed > limits.bodyTimeout+requestTimeout {
			t.Errorf("incomplete headers closed after %s; expected the %s header deadline", elapsed, limits.bodyTimeout)
		}
		gatewayAssertCounter(t, baseline+1)
	})

	t.Run("body-and-metadata-limits", func(t *testing.T) {
		baseline := gatewayCounter(t)
		conn, reader := gatewayOpenRequest(t, gatewayAddress, http.MethodGet, "/counter",
			fmt.Sprintf("Content-Length: %d\r\nExpect: 100-continue\r\n", limits.bodyBytes+1), requestTimeout)
		gatewayExpectStatus(t, reader, http.StatusRequestEntityTooLarge)
		conn.Close()

		conn, reader = gatewayOpenRequest(t, gatewayAddress, http.MethodGet, "/counter",
			"Transfer-Encoding: chunked\r\n", requestTimeout)
		body := strings.Repeat("x", limits.bodyBytes+1)
		writeResult := make(chan error, 1)
		go func() {
			_, err := fmt.Fprintf(conn, "%x\r\n%s\r\n0\r\n\r\n", len(body), body)
			writeResult <- err
		}()
		gatewayExpectStatus(t, reader, http.StatusRequestEntityTooLarge)
		conn.Close()
		// Rejection can close the connection before the last chunk is written.
		<-writeResult

		conn, reader = gatewayOpenRequest(t, gatewayAddress, http.MethodGet, "/counter",
			"X-Large-Header: "+strings.Repeat("x", 20*1024)+"\r\n", requestTimeout)
		gatewayExpectStatus(t, reader, http.StatusRequestHeaderFieldsTooLarge)
		conn.Close()
		gatewayAssertCounter(t, baseline+1)

		baseline = gatewayCounter(t)
		conn, reader = gatewayOpenRequest(t, gatewayAddress, http.MethodGet, "/counter",
			fmt.Sprintf("Content-Length: %d\r\nExpect: 100-continue\r\n", limits.bodyBytes), requestTimeout)
		gatewayExpectStatus(t, reader, http.StatusContinue)
		if _, err := io.WriteString(conn, strings.Repeat("x", limits.bodyBytes)); err != nil {
			t.Fatalf("uploading a body at the limit: %v", err)
		}
		gatewayExpectStatus(t, reader, http.StatusOK)
		conn.Close()
		gatewayAssertCounter(t, baseline+2)
	})

	t.Run("absolute-body-deadline", func(t *testing.T) {
		baseline := gatewayCounter(t)
		started := time.Now()
		conn, reader := gatewayOpenRequest(t, gatewayAddress, http.MethodGet, "/counter",
			"Transfer-Encoding: chunked\r\nExpect: 100-continue\r\n", limits.bodyTimeout+requestTimeout)
		gatewayExpectStatus(t, reader, http.StatusContinue)
		stop := gatewayTrickle(t, conn, limits.bodyTimeout, "1\r\nx\r\n")
		defer stop()
		gatewayExpectStatus(t, reader, http.StatusRequestTimeout)
		stop()
		if elapsed := time.Since(started); elapsed > limits.bodyTimeout+requestTimeout {
			t.Errorf("body timeout took %s; bound is %s", elapsed, limits.bodyTimeout+requestTimeout)
		}
		gatewayAssertCounter(t, baseline+1)
	})

	t.Run("admission-and-release", func(t *testing.T) {
		baseline := gatewayCounter(t)
		var held []net.Conn
		defer func() {
			for _, conn := range held {
				conn.Close()
			}
		}()
		started := time.Now()
		for i := 0; i < limits.inFlight; i++ {
			conn, reader := gatewayOpenRequest(t, gatewayAddress, http.MethodGet, "/counter",
				"Content-Length: 1\r\nExpect: 100-continue\r\n", limits.bodyTimeout+requestTimeout)
			held = append(held, conn)
			// Hyper emits Continue only when the gateway polls the admitted body.
			gatewayExpectStatus(t, reader, http.StatusContinue)
		}
		if elapsed := time.Since(started); elapsed >= limits.bodyTimeout {
			t.Fatalf("saturating intake took %s, exceeding the body deadline %s", elapsed, limits.bodyTimeout)
		}
		conn, reader := gatewayOpenRequest(t, gatewayAddress, http.MethodGet, "/counter",
			"Content-Length: 1\r\nExpect: 100-continue\r\n", min(requestTimeout, limits.bodyTimeout-time.Since(started)))
		gatewayExpectStatus(t, reader, http.StatusServiceUnavailable)
		conn.Close()

		for _, endpoint := range []string{"http://" + gatewayAddress, gatewayMetricsURL} {
			for _, path := range []string{"/healthz", "/readyz"} {
				status, _, err := requestHTTP(t.Context(), testHTTPClient, http.MethodGet,
					endpoint+path, "", nil)
				if err != nil || status != http.StatusOK {
					t.Fatalf("%s%s while intake is full: HTTP %d, error %v", endpoint, path, status, err)
				}
			}
		}
		if count := gatewayInFlight(t); count != float64(limits.inFlight) {
			t.Fatalf("gateway owns %g requests during the saturation check; want %d", count, limits.inFlight)
		}
		for _, conn := range held {
			conn.Close()
		}
		ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
		defer cancel()
		for {
			counter, err := fetchCounter(ctx, "http://"+gatewayAddress+"/counter")
			if err == nil {
				if counter != baseline+1 {
					t.Fatalf("counter after rejected and disconnected uploads = %d; want %d", counter, baseline+1)
				}
				break
			}
			select {
			case <-ctx.Done():
				t.Fatalf("intake did not recover after client disconnects: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
		}
		if count := gatewayInFlight(t); count != 0 {
			t.Fatalf("gateway owns %g requests after disconnect recovery; want zero", count)
		}
	})
}

type gatewayLimits struct {
	inFlight    int
	bodyBytes   int
	bodyTimeout time.Duration
}

func gatewayDeployedLimits(t *testing.T) gatewayLimits {
	t.Helper()
	data, err := kubectlOutput(t.Context(), kubectlTimeout, nil, false,
		"get", "deployment", "gateway", "-n", "wasm-platform", "-o", "json")
	if err != nil {
		t.Fatal("could not read gateway deployment")
	}
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Name string
						Env  []struct{ Name, Value string }
					}
				}
			}
		}
	}
	if err := json.Unmarshal(data, &deployment); err != nil {
		t.Fatal("could not decode gateway deployment")
	}
	values := map[string]int{
		"GATEWAY_MAX_IN_FLIGHT_REQUESTS": 32,
		"GATEWAY_MAX_BODY_BYTES":         128 * 1024,
		"GATEWAY_BODY_READ_TIMEOUT_SECS": 5,
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		if container.Name != "gateway" {
			continue
		}
		for _, env := range container.Env {
			if _, ok := values[env.Name]; !ok {
				continue
			}
			value, err := strconv.Atoi(env.Value)
			if err != nil || value <= 0 {
				t.Fatalf("gateway %s must be a positive integer", env.Name)
			}
			values[env.Name] = value
		}
	}
	limits := gatewayLimits{
		inFlight: values["GATEWAY_MAX_IN_FLIGHT_REQUESTS"], bodyBytes: values["GATEWAY_MAX_BODY_BYTES"],
		bodyTimeout: time.Duration(values["GATEWAY_BODY_READ_TIMEOUT_SECS"]) * time.Second,
	}
	if limits.inFlight > 64 || limits.bodyBytes > maxResponseBytes || limits.bodyTimeout > 30*time.Second {
		t.Fatal("gateway limits exceed the e2e workload bounds")
	}
	return limits
}

func gatewayOpenRequest(t *testing.T, address, method, path, headers string, timeout time.Duration) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, reader := gatewayConnect(t, address, timeout)
	if _, err := fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n%s\r\n", method, path, headers); err != nil {
		t.Fatalf("sending gateway request headers: %v", err)
	}
	return conn, reader
}

func gatewayConnect(t *testing.T, address string, timeout time.Duration) (net.Conn, *bufio.Reader) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		t.Fatalf("connecting to gateway: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatalf("setting gateway connection deadline: %v", err)
	}
	return conn, bufio.NewReader(conn)
}

func gatewayTrickle(t *testing.T, conn net.Conn, timeout time.Duration, data string) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	written := make(chan struct{})
	go func() {
		defer close(written)
		ticker := time.NewTicker(min(250*time.Millisecond, timeout/4))
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := io.WriteString(conn, data); err != nil {
					return
				}
			}
		}
	}()
	return func() {
		cancel()
		conn.Close()
		<-written
	}
}

func gatewayReadResponse(t *testing.T, reader *bufio.Reader) *http.Response {
	t.Helper()
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("gateway did not respond within the request deadline: %v", err)
	}
	return response
}

func gatewayExpectStatus(t *testing.T, reader *bufio.Reader, want int) {
	t.Helper()
	response := gatewayReadResponse(t, reader)
	defer response.Body.Close()
	if response.StatusCode != want {
		t.Fatalf("gateway returned HTTP %d; want %d", response.StatusCode, want)
	}
	if want < http.StatusOK {
		return
	}
	bytes, err := io.Copy(io.Discard, io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		t.Fatalf("reading gateway response: %v", err)
	}
	if bytes > maxResponseBytes {
		t.Fatalf("gateway response exceeds %d bytes", maxResponseBytes)
	}
}

func gatewayCounter(t *testing.T) int {
	t.Helper()
	counter, err := fetchCounter(t.Context(), counterAppURL)
	if err != nil {
		t.Fatalf("counter fixture is unavailable: %v", err)
	}
	return counter
}

func gatewayAssertCounter(t *testing.T, want int) {
	t.Helper()
	if counter := gatewayCounter(t); counter != want {
		t.Fatalf("counter = %d; want %d (rejected input must not dispatch a guest)", counter, want)
	}
}

func gatewayInFlight(t *testing.T) float64 {
	t.Helper()
	status, body, err := requestHTTP(t.Context(), testHTTPClient, http.MethodGet,
		gatewayMetricsURL+"/metrics", "", nil)
	if err != nil || status != http.StatusOK {
		t.Fatalf("gateway metrics: HTTP %d, error %v", status, err)
	}
	const name = "wasm_gateway_work"
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, name+`{pool="in_flight"}`) {
			value, err := metricSum(line, name)
			if err != nil {
				t.Fatalf("gateway in-flight metric: %v", err)
			}
			return value
		}
	}
	t.Fatal("gateway in-flight metric has no sample")
	return 0
}
