//go:build integration

package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func workloadHeaders(t *testing.T, serviceAccount, audience string) http.Header {
	t.Helper()
	token, err := kubectlOutput(t.Context(), kubectlTimeout, nil, false, "create", "token", serviceAccount,
		"-n", "wasm-platform", "--audience="+audience, "--duration=10m")
	if err != nil || len(token) == 0 {
		t.Fatal("could not issue workload test token")
	}
	return http.Header{"Authorization": []string{"Bearer " + strings.TrimSpace(string(token))}}
}

func TestCacheWorkloadIdentity(t *testing.T) {
	endpoint := "http://localhost:3002/modules/sha256:" + strings.Repeat("0", 64)
	for _, test := range []struct {
		name    string
		headers http.Header
		status  int
	}{
		{"anonymous", nil, http.StatusUnauthorized},
		{"invalid", http.Header{"Authorization": []string{"Bearer invalid-workload-token"}}, http.StatusUnauthorized},
		{"wrong-audience", workloadHeaders(t, "execution-host", "wrong-audience"), http.StatusUnauthorized},
		{"wrong-service-account", workloadHeaders(t, "default", "wasm-platform"), http.StatusForbidden},
		{"gateway", workloadHeaders(t, "gateway", "wasm-platform"), http.StatusForbidden},
		{"execution-host", workloadHeaders(t, "execution-host", "wasm-platform"), http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, method := range []string{http.MethodGet, http.MethodPut} {
				want := test.status
				if method == http.MethodPut && test.name == "execution-host" {
					want = http.StatusUnprocessableEntity
				}
				status, _, err := requestHTTP(t.Context(), testHTTPClient, method, endpoint, "invalid-wasm", test.headers)
				if err != nil || status != want {
					t.Fatalf("%s authorization returned HTTP %d, want %d", method, status, want)
				}
			}
		})
	}
}

func TestSyncAuthorityLossCancelsExecutionAndRecovers(t *testing.T) {
	requestForLogRegression(t, testHTTPClient, http.MethodGet, cancellationURL, http.StatusOK, "", "", nil)
	hosts := cancellationHostSnapshots(t, t.Context())
	starts := cancellationStarts(t, hosts)["http:sleep"]
	finished := cancellationMetricTotal(t, hosts, "wasm_host_dispatch_tasks_finished_total", `outcome="cancelled"`)
	token := fmt.Sprintf("authority-loss-%d", time.Now().UnixNano())
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type result struct {
		status int
		err    error
	}
	completed := make(chan result, 1)
	client := &http.Client{Timeout: 30 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	go func() {
		status, _, err := requestHTTP(ctx, client, http.MethodPost, cancellationURL, "sleep "+token+" 120", nil)
		completed <- result{status, err}
	}()
	cancellationEventually(t, time.Now().Add(10*time.Second), func(ctx context.Context) bool {
		return cancellationStarts(t, cancellationHostSnapshots(t, ctx))["http:sleep"] > starts
	}, "authority-loss invocation did not enter the guest")
	restore := func() {
		_, err := kubectlOutput(context.Background(), kubectlTimeout, nil, false,
			"scale", "deployment/wp-operator", "-n", "wasm-platform", "--replicas=1")
		if err != nil {
			t.Error("could not restore configuration operator")
		}
	}
	t.Cleanup(restore)
	kubectlForLogRegression(t, t.Context(), "scale", "deployment/wp-operator", "-n", "wasm-platform", "--replicas=0")
	deadline := time.Now().Add(10 * time.Second)
	cancellationEventually(t, deadline, func(ctx context.Context) bool {
		status, _, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, "http://localhost:3000/cancellation", "", nil)
		return err == nil && status == http.StatusServiceUnavailable
	}, "gateway admitted work after configuration authority was lost")
	cancellationEventually(t, deadline, func(ctx context.Context) bool {
		after := cancellationHostSnapshots(t, ctx)
		for pod, before := range hosts {
			if after[pod].identity != before.identity {
				t.Fatal("execution-host process changed during authority-loss cancellation")
			}
		}
		return cancellationMetricTotal(t, after, "wasm_host_dispatch_tasks_finished_total", `outcome="cancelled"`) > finished &&
			cancellationMetricTotal(t, after, "wasm_host_dispatch_work", `state="tracked"`) == 0
	}, "authority-loss cancellation did not release the actual host execution")
	select {
	case response := <-completed:
		if response.err != nil || response.status != http.StatusServiceUnavailable {
			t.Fatalf("accepted request was not cancelled on authority loss: HTTP %d", response.status)
		}
	case <-ctx.Done():
		t.Fatal("accepted request remained alive after configuration authority loss")
	}
	restore()
	requestForLogRegression(t, testHTTPClient, http.MethodGet, cancellationURL+"?"+token, http.StatusOK, "", "",
		func(body []byte) bool { return string(body) == "sql=0 kv=false" })
}
