package e2e_test

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	requestTimeout   = 5 * time.Second
	kubectlTimeout   = 20 * time.Second
	maxResponseBytes = 4 * 1024 * 1024
)

var testHTTPClient = &http.Client{Timeout: requestTimeout}

func pollContext(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func requestHTTP(ctx context.Context, client *http.Client, method, endpoint, body string, headers http.Header) (int, []byte, error) {
	status, _, data, err := requestHTTPWithHeaders(ctx, client, method, endpoint, body, headers)
	return status, data, err
}

func requestHTTPWithHeaders(ctx context.Context, client *http.Client, method, endpoint, body string, headers http.Header) (int, http.Header, []byte, error) {
	timeout := client.Timeout
	if timeout <= 0 {
		timeout = requestTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	request.Header = headers.Clone()
	response, err := client.Do(request)
	if err != nil {
		return 0, nil, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return response.StatusCode, response.Header, nil, fmt.Errorf("reading response body: %w", err)
	}
	if len(data) > maxResponseBytes {
		return response.StatusCode, response.Header, nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	return response.StatusCode, response.Header, data, nil
}

func commandOutput(ctx context.Context, timeout time.Duration, name string, args []string, input io.Reader, combined bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(ctx, name, args...)
	command.Stdin = input
	command.WaitDelay = time.Second
	var output []byte
	var err error
	if combined {
		output, err = command.CombinedOutput()
	} else {
		output, err = command.Output()
	}
	if ctx.Err() != nil {
		return output, ctx.Err()
	}
	return output, err
}

func kubectlOutput(ctx context.Context, timeout time.Duration, input io.Reader, combined bool, args ...string) ([]byte, error) {
	args = append([]string{"--request-timeout=15s"}, args...)
	return commandOutput(ctx, timeout, "kubectl", args, input, combined)
}

func metricSum(body, name string) (float64, error) {
	var total float64
	var present bool
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		separator := strings.IndexAny(line, "{ \t")
		if separator < 0 {
			separator = len(line)
		}
		if line[:separator] != name {
			continue
		}
		rest := line[separator:]
		if strings.HasPrefix(rest, "{") {
			var quoted, escaped bool
			end := -1
			for i, char := range rest {
				if escaped {
					escaped = false
					continue
				}
				if quoted && char == '\\' {
					escaped = true
				} else if char == '"' {
					quoted = !quoted
				} else if char == '}' && !quoted {
					end = i
					break
				}
			}
			if end < 0 {
				return 0, fmt.Errorf("metric %s has malformed labels", name)
			}
			rest = rest[end+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 || len(fields) > 2 {
			return 0, fmt.Errorf("metric %s has a malformed sample", name)
		}
		value, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, fmt.Errorf("metric %s has an invalid sample value", name)
		}
		total += value
		present = true
	}
	if !present {
		return 0, fmt.Errorf("metric %s has no samples", name)
	}
	return total, nil
}
