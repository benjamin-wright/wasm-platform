package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func testManifest(mediaType, digest string, size int64) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":2},"layers":[{"mediaType":%q,"digest":%q,"size":%d}]}`, digestOf([]byte("{}")), mediaType, digest, size))
}

func testManifestWithArtifactType(artifactType, mediaType, digest string, size int64) []byte {
	return []byte(strings.Replace(string(testManifest(mediaType, digest, size)), `"schemaVersion":2`, fmt.Sprintf(`"schemaVersion":2,"artifactType":%q`, artifactType), 1))
}

func digestOf(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func TestRegistryPolicy(t *testing.T) {
	for _, test := range []struct {
		name, approved, plaintext string
		valid                     bool
	}{
		{"required", "", "", false},
		{"HTTPS", "registry.example.com", "", true},
		{"local", "registry.localhost:5001", "registry.localhost:5001", true},
		{"loopback", "127.0.0.1:5001,[::1]:5002", "127.0.0.1:5001,[::1]:5002", true},
		{"external plaintext", "registry.example.com", "registry.example.com", false},
		{"subset", "registry.localhost:5001", "other.localhost:5001", false},
		{"scheme", "https://registry.example.com", "", false},
		{"credentials", "user:POLICY_SECRET@registry.example.com", "", false},
		{"path", "registry.example.com/repo", "", false},
		{"port", "registry.example.com:99999", "", false},
		{"empty entry", "registry.example.com,", "", false},
		{"uppercase", "Registry.example.com", "", false},
		{"whitespace", " registry.example.com", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewRegistry(test.approved, test.plaintext)
			if (err == nil) != test.valid {
				t.Fatalf("valid=%v, error=%v", test.valid, err)
			}
			if err != nil && strings.Contains(err.Error(), "POLICY_SECRET") {
				t.Fatal("configuration error exposed credentials")
			}
		})
	}
}

func TestRegistryResolvesVerifiedManifest(t *testing.T) {
	wasmDigest := digestOf([]byte("raw wasm bytes"))
	data := testManifest("application/wasm", wasmDigest, 14)
	manifestDigest := digestOf(data)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/v2/demo/manifests/latest" && r.URL.Path != "/v2/demo/manifests/"+manifestDigest {
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("registry request must be anonymous")
		}
		w.Header().Set("Docker-Content-Digest", manifestDigest)
		_, _ = w.Write(data)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	registry, err := NewRegistry(authority, authority)
	if err != nil {
		t.Fatal(err)
	}
	for _, selector := range []string{":latest", "@" + manifestDigest} {
		identity, err := registry.Resolve(t.Context(), "oci://"+authority+"/demo"+selector)
		if err != nil {
			t.Fatal(err)
		}
		if identity.ModuleRef != "oci://"+authority+"/demo@"+manifestDigest || identity.ManifestDigest != manifestDigest || identity.WasmDigest != wasmDigest {
			t.Fatalf("wrong identity: %+v", identity)
		}
	}
	if requests.Load() != 2 {
		t.Fatalf("expected two manifest requests, got %d", requests.Load())
	}
}

func TestRegistryAcceptsDeclaredWasmWithORASLayerMetadata(t *testing.T) {
	wasm := []byte{0, 'a', 's', 'm', 13, 0, 1, 0}
	wasmDigest := digestOf(wasm)
	data := testManifestWithArtifactType("application/vnd.wasm.content.layer.v1+wasm", "application/vnd.oci.image.layer.v1.tar", wasmDigest, int64(len(wasm)))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/demo/manifests/latest" {
			t.Errorf("unexpected registry operation %s", r.URL.Path)
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	registry, err := NewRegistry(authority, authority)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := registry.Resolve(t.Context(), authority+"/demo:latest")
	if err != nil {
		t.Fatal(err)
	}
	if identity.ModuleRef != "oci://"+authority+"/demo@"+digestOf(data) || identity.WasmDigest != wasmDigest {
		t.Fatalf("ORAS metadata did not retain the immutable raw Wasm identity: %+v", identity)
	}
}

func TestRegistryRejectsUnverifiedAndOversizedManifests(t *testing.T) {
	for _, test := range []struct {
		name, body, header string
		status             int
		selector           string
	}{
		{name: "modified pinned manifest", body: string(testManifest("application/wasm", digestOf([]byte("layer")), 8)), selector: "@" + digestOf([]byte("different manifest"))},
		{name: "wrong digest header", body: string(testManifest("application/wasm", digestOf([]byte("layer")), 8)), header: digestOf([]byte("different manifest"))},
		{name: "oversized manifest", body: strings.Repeat("x", MaxManifestBytes+1)},
		{name: "oversized Wasm", body: string(testManifest("application/wasm", digestOf([]byte("layer")), MaxWasmBytes+1))},
		{name: "negative size", body: string(testManifest("application/wasm", digestOf([]byte("layer")), -1))},
		{name: "short Wasm", body: string(testManifest("application/wasm", digestOf([]byte("layer")), 7))},
		{name: "wrong media type", body: string(testManifest("application/vnd.native", digestOf([]byte("layer")), 8))},
		{name: "tar without Wasm declaration", body: string(testManifest("application/vnd.oci.image.layer.v1.tar", digestOf([]byte("layer")), 8))},
		{name: "tar with another artifact declaration", body: string(testManifestWithArtifactType("application/vnd.other.artifact", "application/vnd.oci.image.layer.v1.tar", digestOf([]byte("layer")), 8))},
		{name: "invalid layer digest", body: string(testManifest("application/wasm", "sha256:short", 8))},
		{name: "empty layers", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[]}`},
		{name: "index", body: `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[]}`},
		{name: "external layer URL", body: fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":2},"layers":[{"mediaType":"application/wasm","digest":%q,"size":8,"urls":["https://unapproved.example/REMOTE_SECRET"]}]}`, digestOf([]byte("{}")), digestOf([]byte("layer")))},
		{name: "missing config", body: fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","layers":[{"mediaType":"application/wasm","digest":%q,"size":8}]}`, digestOf([]byte("layer")))},
		{name: "invalid config digest", body: strings.Replace(string(testManifest("application/wasm", digestOf([]byte("layer")), 8)), digestOf([]byte("{}")), "sha256:short", 1)},
		{name: "missing config size", body: strings.Replace(string(testManifest("application/wasm", digestOf([]byte("layer")), 8)), `,"size":2`, "", 1)},
		{name: "negative config size", body: strings.Replace(string(testManifest("application/wasm", digestOf([]byte("layer")), 8)), `"size":2`, `"size":-1`, 1)},
		{name: "empty config media type", body: strings.Replace(string(testManifest("application/wasm", digestOf([]byte("layer")), 8)), "application/vnd.oci.empty.v1+json", "", 1)},
		{name: "authentication required", status: http.StatusUnauthorized, body: "REMOTE_SECRET"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Docker-Content-Digest", test.header)
				if test.status != 0 {
					w.WriteHeader(test.status)
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			authority := strings.TrimPrefix(server.URL, "http://")
			registry, err := NewRegistry(authority, authority)
			if err != nil {
				t.Fatal(err)
			}
			selector := test.selector
			if selector == "" {
				selector = ":latest"
			}
			_, err = registry.Resolve(t.Context(), authority+"/demo"+selector)
			if err == nil || strings.Contains(err.Error(), "REMOTE_SECRET") {
				t.Fatalf("expected safe rejection, got %v", err)
			}
		})
	}
}

func TestRegistryRejectsRedirectsAndImplicitPlaintext(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		_, _ = w.Write(testManifest("application/wasm", digestOf([]byte("layer")), 8))
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/REMOTE_SECRET", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	for _, plaintext := range []string{"", authority} {
		registry, err := NewRegistry(authority, plaintext)
		if err != nil {
			t.Fatal(err)
		}
		if registry.client.Timeout != RequestTimeout {
			t.Fatal("registry client must have a finite deadline")
		}
		_, err = registry.Resolve(t.Context(), authority+"/demo:latest")
		if err == nil || strings.Contains(err.Error(), "REMOTE_SECRET") {
			t.Fatalf("expected safe transport rejection, got %v", err)
		}
	}
	if redirected.Load() != 0 {
		t.Fatal("registry client followed a redirect")
	}
}

func TestRegistryRejectsUnapprovedReferenceBeforeNetwork(t *testing.T) {
	registry, err := NewRegistry("registry.example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		"unapproved.example/demo:latest", "oci://registry.example.com/demo@sha256:short",
		"https://registry.example.com/demo:latest", "oci://user:REFERENCE_SECRET@registry.example.com/demo:latest",
		"oci://registry.example.com/demo:latest?token=REFERENCE_SECRET",
		"registry.example.com/" + strings.Repeat("r", 256) + ":latest",
	} {
		_, err := registry.Resolve(context.Background(), value)
		if err == nil || strings.Contains(err.Error(), "REFERENCE_SECRET") || strings.Contains(err.Error(), value) {
			t.Fatalf("expected safe reference rejection, got %v", err)
		}
	}
}
