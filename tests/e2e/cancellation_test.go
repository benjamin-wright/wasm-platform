//go:build integration

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const cancellationURL = "http://localhost/cancellation"

// TestInvocationCancellationEndToEnd checks stored effects and actual execution
// exit independently of the gateway response, which can expire before the host.
func TestInvocationCancellationEndToEnd(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	kubectlForLogRegression(t, t.Context(), "wait", "application", "cancellation-app", "-n", "default",
		"--for=condition=Ready", "--timeout=15s")
	requestForLogRegression(t, client, http.MethodGet, cancellationURL, http.StatusOK, "", "",
		func(body []byte) bool { return string(body) == "ready" })
	completedToken := fmt.Sprintf("successful-%d", time.Now().UnixNano())
	requestForLogRegression(t, client, http.MethodPost, cancellationURL, http.StatusOK,
		"write "+completedToken+" 0", "", nil)
	requestForLogRegression(t, client, http.MethodGet, cancellationURL+"?"+completedToken, http.StatusOK, "", "",
		func(body []byte) bool { return string(body) == "sql=1 kv=true" })
	requestForLogRegression(t, client, http.MethodPost, cancellationURL, http.StatusOK,
		"transaction rejected-controls 0", "", nil)

	timeout := cancellationHostTimeout(t)
	slowClient := &http.Client{Timeout: timeout + 15*time.Second}
	t.Cleanup(slowClient.CloseIdleConnections)
	delay := timeout + 5*time.Second
	longSleep := max(120*time.Second, timeout+60*time.Second)
	type action struct {
		world, mode, token string
		delay              time.Duration
	}
	var actions []action
	for _, world := range []string{"http", "message"} {
		for _, mode := range []string{"cpu", "sleep", "sql", "committed"} {
			actionDelay := delay
			if mode == "sql" {
				actionDelay = timeout + 250*time.Millisecond
			}
			actions = append(actions, action{
				world: world, mode: mode,
				token: fmt.Sprintf("%s-%s-%d", world, mode, time.Now().UnixNano()),
				delay: actionDelay,
			})
		}
		actions = append(actions, action{
			world: world, mode: "sleep",
			token: fmt.Sprintf("%s-long-sleep-%d", world, time.Now().UnixNano()),
			delay: longSleep,
		})
	}

	startedBefore := cancellationStarts(t, t.Context(), client)
	stoppedBefore := cancellationStops(t, t.Context())
	type result struct {
		action action
		status int
		err    error
	}
	results := make(chan result, len(actions))
	var requests sync.WaitGroup
	startedAt := time.Now()
	requestCtx, cancelRequests := context.WithTimeout(t.Context(), timeout+15*time.Second)
	t.Cleanup(cancelRequests)
	for _, invocation := range actions {
		requests.Add(1)
		go func(a action) {
			defer requests.Done()
			command := fmt.Sprintf("%s %s %.3f", a.mode, a.token, a.delay.Seconds())
			if a.world == "message" {
				command = "message " + command
			}
			status, _, err := cancellationRequest(requestCtx,
				slowClient, http.MethodPost, cancellationURL, command)
			results <- result{action: a, status: status, err: err}
		}(invocation)
	}

	for _, world := range []string{"http", "message"} {
		for _, mode := range []string{"cpu", "sleep", "sql", "committed"} {
			want := startedBefore[world+":"+mode] + 1
			if mode == "sleep" {
				want++
			}
			cancellationEventually(t, startedAt.Add(15*time.Second), func(ctx context.Context) bool {
				return cancellationStarts(t, ctx, client)[world+":"+mode] >= want
			}, world+" "+mode+" never entered the production guest")
		}
	}
	for _, a := range actions {
		if a.mode == "committed" {
			cancellationEventually(t, startedAt.Add(min(15*time.Second, timeout)), func(ctx context.Context) bool {
				status, body, err := cancellationRequest(ctx, client, http.MethodGet,
					cancellationURL+"?"+a.token, "")
				return err == nil && status == http.StatusOK && body == "sql=1 kv=true"
			}, a.world+" writes were not committed before the invocation deadline")
		}
	}

	for _, world := range []string{"http", "message"} {
		cancellationEventually(t, startedAt.Add(timeout+10*time.Second), func(ctx context.Context) bool {
			return cancellationStops(t, ctx)[world] >= stoppedBefore[world]+5
		}, world+" executions did not actually exit within the cancellation bound")
	}

	requests.Wait()
	close(results)
	for result := range results {
		if result.err != nil {
			t.Errorf("%s %s request failed: %v", result.action.world, result.action.mode, result.err)
			continue
		}
		if result.action.world == "message" {
			if result.status != http.StatusOK {
				t.Errorf("message dispatch returned HTTP %d", result.status)
			}
		} else if result.status != http.StatusBadGateway && result.status != http.StatusGatewayTimeout {
			t.Errorf("expired %s invocation returned HTTP %d", result.action.mode, result.status)
		}
	}

	// Observe beyond the delayed SQL commit/sleep time so a timeout response alone
	// cannot hide a surviving worker or a database statement still committing.
	if remaining := time.Until(startedAt.Add(delay + 5*time.Second)); remaining > 0 {
		time.Sleep(remaining)
	}
	effects := append(actions, action{world: "http", mode: "write", token: completedToken})
	for _, a := range effects {
		want := "sql=0 kv=false"
		if a.mode == "committed" || a.mode == "write" {
			want = "sql=1 kv=true"
		}
		status, body, err := cancellationRequest(t.Context(), client, http.MethodGet,
			cancellationURL+"?"+a.token, "")
		if err != nil || status != http.StatusOK || body != want {
			t.Errorf("%s %s stored effects: HTTP %d body=%q err=%v; want %q", a.world, a.mode, status, body, err, want)
		}
	}
}

func cancellationRequest(ctx context.Context, client *http.Client, method, endpoint, body string) (int, string, error) {
	status, data, err := requestHTTP(ctx, client, method, endpoint, body, nil)
	return status, string(data), err
}

func cancellationHostTimeout(t *testing.T) time.Duration {
	t.Helper()
	var deployment struct {
		Spec struct {
			Template struct {
				Spec struct {
					Containers []struct {
						Env []struct{ Name, Value string }
					}
				}
			}
		}
	}
	data := kubectlForLogRegression(t, t.Context(), "get", "deployment", "execution-host", "-n", "wasm-platform", "-o", "json")
	if err := json.Unmarshal(data, &deployment); err != nil {
		t.Fatal("could not decode execution-host deployment")
	}
	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "WASM_TIMEOUT_SECS" {
				seconds, err := strconv.ParseInt(env.Value, 10, 64)
				if err != nil || seconds <= 0 {
					t.Fatal("execution-host has no positive invocation timeout")
				}
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return 30 * time.Second
}

func cancellationStarts(t *testing.T, ctx context.Context, client *http.Client) map[string]int {
	t.Helper()
	status, body, err := cancellationRequest(ctx, client, http.MethodGet, metricsURL, "")
	if err != nil || status != http.StatusOK {
		t.Fatalf("could not read execution-host metrics: HTTP %d err=%v", status, err)
	}
	if _, err := metricSum(body, "cancellation_started_total"); err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	worldLabel := regexp.MustCompile(`world="([^"]+)"`)
	modeLabel := regexp.MustCompile(`mode="([^"]+)"`)
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "cancellation_started_total{") || !strings.Contains(line, `app_name="cancellation-app"`) || !strings.Contains(line, `app_namespace="default"`) {
			continue
		}
		world, mode := worldLabel.FindStringSubmatch(line), modeLabel.FindStringSubmatch(line)
		fields := strings.Fields(line)
		if len(world) == 2 && len(mode) == 2 && len(fields) == 2 {
			count, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				t.Fatal("invalid guest start metric")
			}
			counts[world[1]+":"+mode[1]] += int(count)
		}
	}
	return counts
}

func cancellationStops(t *testing.T, ctx context.Context) map[string]int {
	t.Helper()
	logs := kubectlForLogRegression(t, ctx, "logs", "deployment/execution-host", "-n", "wasm-platform", "--tail=-1")
	counts := make(map[string]int)
	plainLogs := regexp.MustCompile(`\x1b\[[0-9;]*m`).ReplaceAllString(string(logs), "")
	for _, line := range strings.Split(plainLogs, "\n") {
		if !strings.Contains(line, "invocation stopped") || !strings.Contains(line, "cancellation-app") || !strings.Contains(line, "cancelled=true") || !strings.Contains(line, "expired=true") {
			continue
		}
		if strings.Contains(line, "function_name=http") {
			counts["http"]++
		} else if strings.Contains(line, "function_name=messages") {
			counts["message"]++
		}
	}
	return counts
}

func cancellationEventually(t *testing.T, deadline time.Time, condition func(context.Context) bool, failure string) {
	t.Helper()
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	defer cancel()
	for {
		if ctx.Err() != nil {
			t.Fatal(failure)
		}
		matched := condition(ctx)
		if ctx.Err() != nil {
			t.Fatal(failure)
		}
		if matched {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(failure)
		case <-time.After(250 * time.Millisecond):
		}
	}
}
