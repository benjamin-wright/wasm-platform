package e2e_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestMetricSum(t *testing.T) {
	for _, sample := range []struct {
		name, body string
		want       float64
		wantError  bool
	}{
		{"absent", "other_metric 0\n", 0, true},
		{"metadata only", "# HELP work Active work\n# TYPE work gauge\n", 0, true},
		{"zero", "work 0\n", 0, false},
		{"labeled zero", "work{state=\"active\"} 0\n", 0, false},
		{"sum series", "work{state=\"active\"} 2\nwork{state=\"waiting\"} 3\n", 5, false},
		{"exact name", "work_total 19\nwork 0\n", 0, false},
		{"whitespace and timestamp", "  work\t4 1700000000000\n", 4, false},
		{"quoted labels", "work{detail=\"space } and \\\"quote\\\"\"} 2\n", 2, false},
		{"missing value", "work{state=\"active\"} \n", 0, true},
		{"name only", "work\n", 0, true},
		{"invalid value", "work invalid\n", 0, true},
		{"invalid among valid", "work 1\nwork invalid\n", 0, true},
		{"unclosed labels", "work{state=\"active\" 1\n", 0, true},
		{"NaN", "work NaN\n", 0, true},
		{"infinite", "work +Inf\n", 0, true},
	} {
		t.Run(sample.name, func(t *testing.T) {
			got, err := metricSum(sample.body, "work")
			if (err != nil) != sample.wantError {
				t.Fatalf("metricSum error=%v; wantError=%t", err, sample.wantError)
			}
			if err == nil && got != sample.want {
				t.Fatalf("metricSum=%g; want %g", got, sample.want)
			}
		})
	}
}

func TestHTTPRequestHonorsPollingDeadline(t *testing.T) {
	for _, flushHeaders := range []bool{false, true} {
		name := "stalled headers"
		if flushHeaders {
			name = "stalled body"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if flushHeaders {
					w.WriteHeader(http.StatusOK)
					_, _ = io.WriteString(w, "partial")
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
			}))
			t.Cleanup(server.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			started := time.Now()
			_, _, err := requestHTTP(ctx, server.Client(), http.MethodGet, server.URL, "", nil)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("request error=%v; want polling deadline", err)
			}
			if time.Since(started) > time.Second {
				t.Fatal("request outlived the polling deadline by more than one second")
			}
		})
	}
}

func TestHTTPRequestRejectsOversizedBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(w, strings.NewReader(strings.Repeat("x", maxResponseBytes+1)))
	}))
	t.Cleanup(server.Close)
	_, body, err := requestHTTP(t.Context(), server.Client(), http.MethodGet, server.URL, "", nil)
	if err == nil || len(body) != 0 {
		t.Fatal("oversized response was accepted")
	}
}

func TestCommandHonorsDeadline(t *testing.T) {
	for _, shorterParent := range []bool{false, true} {
		name := "operation deadline"
		parentTimeout, operationTimeout := 5*time.Second, 100*time.Millisecond
		if shorterParent {
			name = "polling deadline"
			parentTimeout, operationTimeout = operationTimeout, parentTimeout
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), parentTimeout)
			defer cancel()
			started := time.Now()
			_, err := commandOutput(ctx, operationTimeout, "sh", []string{"-c", "exec sleep 60"}, nil, false)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("command error=%v; want deadline", err)
			}
			if time.Since(started) > 2*time.Second {
				t.Fatal("command outlived its deadline by more than two seconds")
			}
		})
	}
}

func TestCommandBoundsInheritedOutputPipe(t *testing.T) {
	started := time.Now()
	_, err := commandOutput(t.Context(), 5*time.Second, "sh",
		[]string{"-c", "while printf x; do sleep 0.05; done &"}, nil, false)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("command error=%v; want inherited output pipe timeout", err)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("inherited output pipe remained open beyond its wait budget")
	}
}
