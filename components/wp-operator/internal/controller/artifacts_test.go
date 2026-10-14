package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/artifacts"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/routestore"
)

func moduleDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

func moduleManifest(layer string) []byte {
	return []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.empty.v1+json","digest":%q,"size":2},"layers":[{"mediaType":"application/wasm","digest":%q,"size":8}]}`, moduleDigest([]byte("{}")), moduleDigest([]byte(layer))))
}

func artifactReconciler(c client.Client, registry artifacts.Resolver) *ApplicationReconciler {
	return &ApplicationReconciler{Client: c, Scheme: testScheme, Store: configstore.New(), RouteStore: routestore.New(), Artifacts: registry}
}

func artifactApp(moduleRef string) *wasmplatformv1alpha1.Application {
	return &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "artifact-app", Namespace: "default", Generation: 1, Finalizers: []string{applicationFinalizer}},
		Spec:       wasmplatformv1alpha1.ApplicationSpec{Functions: []wasmplatformv1alpha1.FunctionSpec{{Name: "handler", Module: moduleRef, Trigger: wasmplatformv1alpha1.FunctionTrigger{Topic: "artifact.requests"}}}},
	}
}

func reconcileArtifact(t *testing.T, reconciler *ApplicationReconciler) error {
	t.Helper()
	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "artifact-app"}})
	return err
}

func TestReconcilePinsModulesAndReusesPersistedResolution(t *testing.T) {
	manifestA, manifestB := moduleManifest("wasmA"), moduleManifest("wasmB")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		data := manifestA
		if requests.Add(1) > 1 {
			data = manifestB
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	registry, err := artifacts.NewRegistry(authority, authority)
	if err != nil {
		t.Fatal(err)
	}
	app := artifactApp("oci://" + authority + "/demo:latest")
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := artifactReconciler(c, registry)
	if err := reconcileArtifact(t, reconciler); err != nil {
		t.Fatal(err)
	}
	pinnedA := "oci://" + authority + "/demo@" + moduleDigest(manifestA)
	configs := reconciler.Store.Snapshot()
	if len(configs) != 1 || configs[0].Functions[0].ModuleRef != pinnedA {
		t.Fatalf("configuration did not use the immutable module: %+v", configs)
	}
	var persisted wasmplatformv1alpha1.Application
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	if err := c.Get(t.Context(), key, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Status.ResolvedModules) != 1 || persisted.Status.ResolvedModules[0].ModuleRef != pinnedA || persisted.Status.ResolvedModules[0].WasmDigest != moduleDigest([]byte("wasmA")) {
		t.Fatalf("incorrect persisted module identity: %+v", persisted.Status.ResolvedModules)
	}
	if err := reconcileArtifact(t, reconciler); err != nil {
		t.Fatal(err)
	}
	restarted := artifactReconciler(c, registry)
	if err := reconcileArtifact(t, restarted); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || restarted.Store.Snapshot()[0].Functions[0].ModuleRef != pinnedA {
		t.Fatal("reconciliation or restart resolved the mutable tag again")
	}
	if err := c.Get(t.Context(), key, &persisted); err != nil {
		t.Fatal(err)
	}
	persisted.Generation = 2
	persisted.Spec.Env = map[string]string{"VERSION": "new generation"}
	if err := c.Update(t.Context(), &persisted); err != nil {
		t.Fatal(err)
	}
	if err := reconcileArtifact(t, restarted); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 || restarted.Store.Snapshot()[0].Functions[0].ModuleRef != "oci://"+authority+"/demo@"+moduleDigest(manifestB) {
		t.Fatal("new generation did not resolve and publish the changed tag")
	}
}

func TestReconcileArtifactFailureKeepsPublishedConfiguration(t *testing.T) {
	manifest := moduleManifest("wasmA")
	var fail atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("REMOTE_SECRET"))
			return
		}
		_, _ = w.Write(manifest)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	registry, err := artifacts.NewRegistry(authority, authority)
	if err != nil {
		t.Fatal(err)
	}
	app := artifactApp(authority + "/demo:latest")
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := artifactReconciler(c, registry)
	if err := reconcileArtifact(t, reconciler); err != nil {
		t.Fatal(err)
	}
	before := reconciler.Store.Snapshot()[0]
	var changed wasmplatformv1alpha1.Application
	key := types.NamespacedName{Namespace: app.Namespace, Name: app.Name}
	if err := c.Get(t.Context(), key, &changed); err != nil {
		t.Fatal(err)
	}
	changed.Generation = 2
	changed.Spec.Functions[0].Module = authority + "/demo:new"
	if err := c.Update(t.Context(), &changed); err != nil {
		t.Fatal(err)
	}
	fail.Store(true)
	err = reconcileArtifact(t, reconciler)
	if err == nil || strings.Contains(err.Error(), "REMOTE_SECRET") {
		t.Fatalf("expected safe failure, got %v", err)
	}
	if reconciler.Store.Snapshot()[0] != before {
		t.Fatal("failed resolution replaced the working configuration")
	}
	if err := c.Get(t.Context(), key, &changed); err != nil {
		t.Fatal(err)
	}
	if len(changed.Status.ResolvedModules) != 0 {
		t.Fatal("status retained artifact records from an older desired generation")
	}
	if len(changed.Status.Conditions) == 0 || changed.Status.Conditions[0].Status != metav1.ConditionFalse {
		t.Fatal("resolution failure did not mark the desired application unready")
	}
}

func TestReconcileRetainsResolvedModulesDuringPartialFailure(t *testing.T) {
	var firstRequests, secondRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/first/") {
			firstRequests.Add(1)
			_, _ = w.Write(moduleManifest("first"))
			return
		}
		if secondRequests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(moduleManifest("second"))
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	registry, err := artifacts.NewRegistry(authority, authority)
	if err != nil {
		t.Fatal(err)
	}
	app := artifactApp(authority + "/first:latest")
	app.Spec.Functions = append(app.Spec.Functions, wasmplatformv1alpha1.FunctionSpec{Name: "second", Module: authority + "/second:latest", Trigger: wasmplatformv1alpha1.FunctionTrigger{Topic: "artifact.second"}})
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := artifactReconciler(c, registry)
	if err := reconcileArtifact(t, reconciler); err == nil {
		t.Fatal("expected the second module to fail resolution")
	}
	if len(reconciler.Store.Snapshot()) != 0 {
		t.Fatal("partial module resolution was published")
	}
	if err := reconcileArtifact(t, reconciler); err != nil {
		t.Fatal(err)
	}
	if firstRequests.Load() != 1 || secondRequests.Load() != 2 || len(reconciler.Store.Snapshot()[0].Functions) != 2 {
		t.Fatal("retry did not reuse the successful immutable resolution")
	}
}

func TestReconcilePreservesCurrentPinsAcrossPolicyFailures(t *testing.T) {
	var requests atomic.Int32
	var moved atomic.Bool
	first, second := moduleManifest("first"), moduleManifest("second")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		data := second
		if strings.Contains(r.URL.Path, "/first/") {
			data = first
		}
		if moved.Load() {
			data = moduleManifest("moved tag")
		}
		_, _ = w.Write(data)
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	registry, err := artifacts.NewRegistry(authority, authority)
	if err != nil {
		t.Fatal(err)
	}
	app := artifactApp(authority + "/first:latest")
	app.Spec.Functions = append(app.Spec.Functions, wasmplatformv1alpha1.FunctionSpec{Name: "second", Module: authority + "/second:latest", Trigger: wasmplatformv1alpha1.FunctionTrigger{Topic: "artifact.second"}})
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := artifactReconciler(c, registry)
	if err := reconcileArtifact(t, reconciler); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal("expected one initial resolution per function")
	}
	restricted, err := artifacts.NewRegistry("approved.example", "")
	if err != nil {
		t.Fatal(err)
	}
	restarted := artifactReconciler(c, restricted)
	updates := restarted.Store.RegisterHost("observer")
	if err := reconcileArtifact(t, restarted); err == nil {
		t.Fatal("expected the replacement registry policy to reject the source")
	}
	var persisted wasmplatformv1alpha1.Application
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Status.ResolvedModules) != 2 || persisted.Generation != 1 {
		t.Fatal("policy rejection erased accepted pins from the current generation")
	}
	select {
	case <-updates:
		t.Fatal("policy rejection published a configuration")
	default:
	}
	moved.Store(true)
	restarted.Artifacts = registry
	if err := reconcileArtifact(t, restarted); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 2 {
		t.Fatal("policy restoration resolved mutable tags again in the same generation")
	}
	select {
	case update := <-updates:
		functions := update.Updates[0].AppConfig.Functions
		if len(functions) != 2 || functions[0].ModuleRef != "oci://"+authority+"/first@"+moduleDigest(first) || functions[1].ModuleRef != "oci://"+authority+"/second@"+moduleDigest(second) {
			t.Fatal("policy restoration published changed module identities")
		}
	default:
		t.Fatal("policy restoration did not publish the accepted pinned configuration")
	}
	if err := reconcileArtifact(t, restarted); err != nil {
		t.Fatal(err)
	}
	select {
	case <-updates:
		t.Fatal("unchanged pins caused another configuration publication")
	default:
	}
}

type failingStatusClient struct{ client.Client }
type failingStatusWriter struct{ client.SubResourceWriter }

func (c failingStatusClient) Status() client.SubResourceWriter {
	return failingStatusWriter{c.Client.Status()}
}

func (f failingStatusWriter) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return errors.New("status write failed")
}

func TestReconcileRequiresPersistedIdentityBeforePublishing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(moduleManifest("wasmA"))
	}))
	defer server.Close()
	authority := strings.TrimPrefix(server.URL, "http://")
	registry, err := artifacts.NewRegistry(authority, authority)
	if err != nil {
		t.Fatal(err)
	}
	app := artifactApp(authority + "/demo:latest")
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := artifactReconciler(failingStatusClient{c}, registry)
	if err := reconcileArtifact(t, reconciler); err == nil {
		t.Fatal("expected persistence failure")
	}
	if len(reconciler.Store.Snapshot()) != 0 || reconciler.RouteStore.Version() != 0 {
		t.Fatal("unpersisted module identity was published")
	}
}

func TestReconcileRejectsUnapprovedModule(t *testing.T) {
	registry, err := artifacts.NewRegistry("approved.example", "")
	if err != nil {
		t.Fatal(err)
	}
	app := artifactApp("unapproved.example/demo:latest")
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := artifactReconciler(c, registry)
	if err := reconcileArtifact(t, reconciler); err == nil {
		t.Fatal("expected policy rejection")
	}
	if len(reconciler.Store.Snapshot()) != 0 {
		t.Fatal("unapproved module was published")
	}
}
