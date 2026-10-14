//go:build integration

package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
)

type resolvedModule struct {
	Name           string `json:"name"`
	Generation     int64  `json:"generation"`
	RequestedRef   string `json:"requestedRef"`
	ModuleRef      string `json:"moduleRef"`
	ManifestDigest string `json:"manifestDigest"`
	WasmDigest     string `json:"wasmDigest"`
}

func TestVerifiedRawWasmCacheAndLocalCompilation(t *testing.T) {
	g := NewWithT(t)
	data, err := kubectlOutput(t.Context(), kubectlTimeout, nil, false, "get", "application", "demo-app", "-n", "examples", "-o", "json")
	g.Expect(err).NotTo(HaveOccurred())
	var fixture struct {
		Metadata struct {
			Generation int64 `json:"generation"`
		} `json:"metadata"`
		Status struct {
			ResolvedModules []resolvedModule `json:"resolvedModules"`
		} `json:"status"`
	}
	g.Expect(json.Unmarshal(data, &fixture)).To(Succeed())
	var module resolvedModule
	for _, candidate := range fixture.Status.ResolvedModules {
		if candidate.Name == "http-handler" {
			module = candidate
			break
		}
	}
	g.Expect(module.Name).To(Equal("http-handler"))
	g.Expect(module.Generation).To(Equal(fixture.Metadata.Generation))
	g.Expect(module.ModuleRef).To(HaveSuffix("@" + module.ManifestDigest))

	cacheURL := "http://localhost:3002/modules/" + module.WasmDigest
	var raw []byte
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (int, error) {
		status, body, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, cacheURL, "", nil)
		raw = body
		return status, err
	}, routeTimeout, pollInterval).Should(Equal(http.StatusOK))
	g.Expect(bytes.HasPrefix(raw, []byte{0, 'a', 's', 'm'})).To(BeTrue())
	g.Expect(fmt.Sprintf("sha256:%x", sha256.Sum256(raw))).To(Equal(module.WasmDigest))
	status, _, err := requestHTTP(t.Context(), testHTTPClient, http.MethodPut, cacheURL, "\x7fELF untrusted-native-artifact", nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(status).To(Equal(http.StatusUnprocessableEntity))
	status, body, err := requestHTTP(t.Context(), testHTTPClient, http.MethodGet, cacheURL, "", nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(status).To(Equal(http.StatusOK))
	g.Expect(body).To(Equal(raw))
	status, _, err = requestHTTP(t.Context(), testHTTPClient, http.MethodGet, cacheURL+"/aarch64/42", "", nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(status).To(Equal(http.StatusNotFound))

	var preparationBaseline map[string]float64
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (bool, error) {
		samples, err := scrapePreparationMetrics(ctx)
		if err != nil {
			return false, err
		}
		preparationBaseline = samples
		return true, preparationIdle(samples)
	}, routeTimeout, pollInterval).Should(BeTrue(), "preparation work and permit occupancy should settle before adding an application")
	for kind, limit := range map[string]float64{"queued": 17, "active": 1, "tracked": 17, "compiling": 1} {
		g.Expect(preparationBaseline[`limit{kind="`+kind+`"}`]).To(Equal(limit), kind+" preparation limit")
	}

	name := "artifact-cache-check"
	applyArtifactApplication(t, name, module.ModuleRef, "/artifact-cache-check", true)
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (int, error) {
		status, body, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, "http://localhost/artifact-cache-check", "", nil)
		if status == http.StatusOK && !strings.Contains(string(body), "hello from wasm") {
			return status, fmt.Errorf("unexpected guest response")
		}
		return status, err
	}, routeTimeout, pollInterval).Should(Equal(http.StatusOK))
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (bool, error) {
		_, metrics, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, "http://localhost:9090/metrics", "", nil)
		if err != nil {
			return false, err
		}
		for _, line := range strings.Split(string(metrics), "\n") {
			if strings.HasPrefix(line, "wasm_host_module_compilations_total{") && strings.Contains(line, `app_name="artifact-cache-check"`) && strings.Contains(line, `result="ok"`) {
				return true, nil
			}
		}
		return false, nil
	}, routeTimeout, pollInterval).Should(BeTrue(), "cache hits must still compile locally")
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (bool, error) {
		samples, err := scrapePreparationMetrics(ctx)
		if err != nil {
			return false, err
		}
		for _, sample := range []string{
			`submissions_total{outcome="admitted"}`,
			`finished_total{outcome="success"}`,
			`compilation_finished_total{outcome="success"}`,
			"admission_wait_seconds_count", "queue_wait_seconds_count", "compilation_seconds_count", "compilation_seconds_sum",
		} {
			if samples[sample] <= preparationBaseline[sample] {
				return false, fmt.Errorf("preparation sample %s did not increase: before=%g, after=%g", sample, preparationBaseline[sample], samples[sample])
			}
		}
		for _, kind := range []string{"queued", "active", "tracked", "compiling"} {
			if occupancy := samples[`occupancy{kind="`+kind+`"}`]; occupancy < 0 || occupancy > samples[`limit{kind="`+kind+`"}`] {
				return false, fmt.Errorf("%s preparation occupancy is outside its limit: %g", kind, occupancy)
			}
			if samples[`high_water{kind="`+kind+`"}`] > samples[`limit{kind="`+kind+`"}`] {
				return false, fmt.Errorf("%s preparation high-water exceeds its limit", kind)
			}
		}
		if err := preparationIdle(samples); err != nil {
			return false, err
		}
		return true, nil
	}, routeTimeout, pollInterval).Should(BeTrue(), "cached compilation should record its lifetimes and release preparation capacity")
}

func scrapePreparationMetrics(ctx context.Context) (map[string]float64, error) {
	status, body, err := requestHTTP(ctx, testHTTPClient, http.MethodGet, metricsURL, "", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("preparation metrics: HTTP %d", status)
	}
	var samples []string
	for _, state := range []string{"admission_wait", "queued", "preparing", "compiling", "terminating"} {
		samples = append(samples, `work{state="`+state+`"}`)
	}
	for _, kind := range []string{"queued", "active", "tracked", "compiling"} {
		samples = append(samples, `limit{kind="`+kind+`"}`, `occupancy{kind="`+kind+`"}`, `high_water{kind="`+kind+`"}`)
	}
	for _, outcome := range []string{"admitted", "timeout", "cancelled", "closed"} {
		samples = append(samples, `submissions_total{outcome="`+outcome+`"}`)
	}
	for _, outcome := range []string{"success", "error", "timeout", "cancelled", "panic"} {
		samples = append(samples, `finished_total{outcome="`+outcome+`"}`, `compilation_finished_total{outcome="`+outcome+`"}`)
	}
	for _, histogram := range []string{"admission_wait_seconds", "queue_wait_seconds", "compilation_seconds", "compilation_overrun_seconds"} {
		samples = append(samples, histogram+"_count", histogram+"_sum")
	}
	values := make(map[string]float64, len(samples))
	lines := strings.Split(string(body), "\n")
	for _, sample := range samples {
		name := "wasm_host_preparation_" + sample
		var matching []string
		for _, line := range lines {
			if strings.HasPrefix(line, name+" ") {
				matching = append(matching, line)
			}
		}
		baseName := strings.SplitN(name, "{", 2)[0]
		value, err := metricSum(strings.Join(matching, "\n"), baseName)
		if err != nil {
			return nil, fmt.Errorf("preparation sample %s: %w", sample, err)
		}
		values[sample] = value
	}
	return values, nil
}

func preparationIdle(samples map[string]float64) error {
	for _, state := range []string{"admission_wait", "queued", "preparing", "compiling", "terminating"} {
		if work := samples[`work{state="`+state+`"}`]; work != 0 {
			return fmt.Errorf("%s preparation work has not returned to idle: %g", state, work)
		}
	}
	for _, kind := range []string{"queued", "active", "tracked", "compiling"} {
		if occupancy := samples[`occupancy{kind="`+kind+`"}`]; occupancy != 0 {
			return fmt.Errorf("%s preparation occupancy has not returned to idle: %g", kind, occupancy)
		}
	}
	return nil
}

func TestUnapprovedRegistryCannotPublishConfiguration(t *testing.T) {
	g := NewWithT(t)
	applyArtifactApplication(t, "artifact-denied", "oci://unapproved.invalid/module:latest", "/artifact-denied", false)
	g.Eventually(pollContext(t, routeTimeout), func(ctx context.Context) (bool, error) {
		data, err := kubectlOutput(ctx, kubectlTimeout, nil, false, "get", "application", "artifact-denied", "-n", "examples", "-o", "json")
		if err != nil {
			return false, err
		}
		var app struct {
			Status struct {
				Conditions      []struct{ Type, Status, Message string }
				ResolvedModules []resolvedModule `json:"resolvedModules"`
			}
		}
		if err := json.Unmarshal(data, &app); err != nil {
			return false, err
		}
		for _, condition := range app.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "False" && strings.Contains(condition.Message, "not approved") {
				return len(app.Status.ResolvedModules) == 0, nil
			}
		}
		return false, nil
	}, routeTimeout, pollInterval).Should(BeTrue())
	status, _, err := requestHTTP(t.Context(), testHTTPClient, http.MethodGet, "http://localhost/artifact-denied", "", nil)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(status).To(Equal(http.StatusNotFound))
}

func applyArtifactApplication(t *testing.T, name, moduleRef, path string, kv bool) {
	t.Helper()
	t.Cleanup(func() {
		_, err := kubectlOutput(context.Background(), kubectlTimeout, nil, true, "delete", "application", name, "-n", "examples", "--ignore-not-found", "--wait=false")
		if err != nil {
			t.Errorf("removing artifact test application: %v", err)
		}
	})
	manifest := map[string]any{
		"apiVersion": "wasm-platform.io/v1alpha1", "kind": "Application",
		"metadata": map[string]any{"name": name, "namespace": "examples"},
		"spec":     map[string]any{"kv": kv, "functions": []any{map[string]any{"name": "handler", "module": moduleRef, "trigger": map[string]any{"http": map[string]any{"path": path, "methods": []string{"GET"}}}}}},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := kubectlOutput(t.Context(), kubectlTimeout, bytes.NewReader(data), true, "apply", "-f", "-"); err != nil {
		t.Fatalf("applying artifact fixture: %v: %s", err, out)
	}
}
