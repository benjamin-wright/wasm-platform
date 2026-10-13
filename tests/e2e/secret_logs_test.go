//go:build integration

package e2e_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProductionLogsExcludeCredentials exercises the provisioned Redis, NATS,
// and PostgreSQL connections, then checks the deployed components' logs against
// the actual credentials. Assertion failures must never include captured logs
// or credential values.
func TestProductionLogsExcludeCredentials(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	requestForLogRegression(t, client, http.MethodGet, baseURL, http.StatusOK, "", "", func(body []byte) bool {
		counts, err := parseCounters(string(body))
		return err == nil && counts.requests > 0 && counts.messages > 0
	})
	requestForLogRegression(t, client, http.MethodPost, sqlHelloSetupURL, http.StatusOK, "", "", nil)
	requestForLogRegression(t, client, http.MethodGet, sqlHelloURL, http.StatusOK, "", "", func(body []byte) bool {
		return strings.Contains(string(body), `"name":"Alice"`)
	})
	requestForLogRegression(t, client, http.MethodPost, sqlHelloInsertURL, http.StatusForbidden, "", "", nil)

	type credentialRef struct {
		namespace, name, key string
	}
	forbidden := invokeGuestErrorForLogRegression(t, client)
	for _, ref := range []credentialRef{
		{"wasm-platform", "execution-host-redis-credentials", "REDIS_PASSWORD"},
		{"wasm-platform", "execution-host-nats-credentials", "NATS_PASSWORD"},
		{"wasm-platform", "gateway-nats-credentials", "NATS_PASSWORD"},
		{"default", "wasm-default-sql-hello-writer-pg-creds", "PGPASSWORD"},
		{"default", "wasm-default-sql-hello-reader-pg-creds", "PGPASSWORD"},
	} {
		out := kubectlForLogRegression(t, t.Context(), "get", "secret", ref.name, "-n", ref.namespace, "-o", "json")
		var secret struct {
			Data map[string][]byte `json:"data"`
		}
		if err := json.Unmarshal(out, &secret); err != nil {
			t.Fatalf("could not decode credential Secret %s/%s", ref.namespace, ref.name)
		}
		value := string(secret.Data[ref.key])
		if value == "" {
			t.Fatalf("credential Secret %s/%s is missing %s", ref.namespace, ref.name, ref.key)
		}
		quoted := strconv.Quote(value)
		forbidden = append(forbidden,
			value,
			url.QueryEscape(value),
			url.PathEscape(value),
			strings.TrimPrefix(url.UserPassword("", value).String(), ":"),
			base64.StdEncoding.EncodeToString([]byte(value)),
			quoted[1:len(quoted)-1],
		)
	}

	for _, component := range []struct {
		name    string
		markers []string
	}{
		{"execution-host", []string{"execution-host starting", "Redis client configured from configsync", "SQL pool created", "connected to NATS"}},
		{"gateway", []string{"gateway starting", "connected to NATS"}},
		{"module-cache", []string{"module-cache starting"}},
		{"wp-operator", []string{"starting manager", "application config pushed"}},
	} {
		t.Run(component.name, func(t *testing.T) {
			out := kubectlForLogRegression(t, t.Context(), "get", "pods", "-n", "wasm-platform",
				"-l", "app.kubernetes.io/name="+component.name, "-o", "json")
			var pods struct {
				Items []struct {
					Metadata struct {
						Name string `json:"name"`
					} `json:"metadata"`
					Spec struct {
						Containers []struct {
							Name string `json:"name"`
						} `json:"containers"`
					} `json:"spec"`
					Status struct {
						ContainerStatuses []struct {
							Name         string `json:"name"`
							RestartCount int    `json:"restartCount"`
						} `json:"containerStatuses"`
					} `json:"status"`
				} `json:"items"`
			}
			if err := json.Unmarshal(out, &pods); err != nil {
				t.Fatal("could not decode component pod list")
			}
			if len(pods.Items) == 0 {
				t.Fatal("no deployed component pods found")
			}
			var componentLogs strings.Builder
			for _, pod := range pods.Items {
				if len(pod.Spec.Containers) == 0 {
					t.Fatalf("pod %s has no containers", pod.Metadata.Name)
				}
				for _, container := range pod.Spec.Containers {
					args := []string{"logs", pod.Metadata.Name, "-n", "wasm-platform", "-c", container.Name, "--tail=-1"}
					logs := kubectlForLogRegression(t, t.Context(), args...)
					assertLogsExcludeCredentials(t, logs, forbidden, pod.Metadata.Name, container.Name)
					componentLogs.Write(logs)
					for _, status := range pod.Status.ContainerStatuses {
						if status.Name == container.Name && status.RestartCount > 0 {
							previous := kubectlForLogRegression(t, t.Context(), append(args, "--previous")...)
							assertLogsExcludeCredentials(t, previous, forbidden, pod.Metadata.Name, container.Name)
						}
					}
				}
			}
			for _, marker := range component.markers {
				if !strings.Contains(componentLogs.String(), marker) {
					t.Fatalf("production log marker %q was not observed", marker)
				}
			}
			if component.name == "execution-host" {
				var failureObserved bool
				for _, line := range strings.Split(componentLogs.String(), "\n") {
					if strings.Contains(line, "invocation failed") && strings.Contains(line, "log-error") {
						failureObserved = true
						break
					}
				}
				if !failureObserved {
					t.Fatal("guest invocation failure was not observed in execution-host logs")
				}
			}
		})
	}
}

func invokeGuestErrorForLogRegression(t *testing.T, client *http.Client) []string {
	t.Helper()
	const (
		endpoint = "http://localhost/log-error"
		token    = "f04-dummy-authorization-token-must-stay-private"
		password = "f04-dummy-body-password-must-stay-private"
	)
	body := requestForLogRegression(t, client, http.MethodPost, endpoint, http.StatusInternalServerError,
		`{"password":"`+password+`"}`, "Bearer "+token, nil)
	if string(body) != "internal error: invocation failed" {
		t.Fatal("guest error response did not contain the expected safe message (content withheld)")
	}
	return []string{token, password}
}

func requestForLogRegression(t *testing.T, client *http.Client, method, endpoint string, status int, body, authorization string, validBody func([]byte) bool) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), routeTimeout)
	defer cancel()
	headers := make(http.Header)
	if authorization != "" {
		headers.Set("Authorization", authorization)
	}
	for {
		gotStatus, data, err := requestHTTP(ctx, client, method, endpoint, body, headers)
		if err == nil && gotStatus == status && (validBody == nil || validBody(data)) {
			return data
		}
		select {
		case <-ctx.Done():
			t.Fatalf("%s %s did not exercise the expected production path within %s", method, endpoint, routeTimeout)
		case <-time.After(pollInterval):
		}
	}
}

func kubectlForLogRegression(t *testing.T, ctx context.Context, args ...string) []byte {
	t.Helper()
	out, err := kubectlOutput(ctx, kubectlTimeout, nil, false, args...)
	if err != nil {
		// kubectl stderr can include server-supplied content; retain only the
		// operation name, never subprocess output, in the test failure.
		t.Fatalf("kubectl %s failed while checking production logs", args[0])
	}
	return out
}

func assertLogsExcludeCredentials(t *testing.T, logs []byte, forbidden []string, pod, container string) {
	t.Helper()
	for _, value := range forbidden {
		if strings.Contains(string(logs), value) {
			t.Errorf("credential material appeared in logs from pod %s container %s (content withheld)", pod, container)
			return
		}
	}
}
