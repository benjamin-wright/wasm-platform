package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/artifacts"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/authority"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func startupRegistry(t *testing.T, unavailable *atomic.Bool) (artifacts.Resolver, string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if unavailable != nil && unavailable.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(moduleManifest("wasmA"))
	}))
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	registry, err := artifacts.NewRegistry(host, host)
	if err != nil {
		t.Fatal(err)
	}
	return registry, host + "/demo:latest"
}

func TestStartupAuthorityWaitsForActualReconciliation(t *testing.T) {
	var unavailable atomic.Bool
	unavailable.Store(true)
	registry, moduleRef := startupRegistry(t, &unavailable)
	app := artifactApp(moduleRef)
	app.UID = "startup-app"
	app.Finalizers = nil
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := artifactReconciler(c, registry)
	publication := authority.New()
	reconciler.Authority = publication
	publication.Initialize(t.Context(), []wasmplatformv1alpha1.Application{*app})
	if err := reconcileArtifact(t, reconciler); err != nil {
		t.Fatal(err)
	}
	if publication.Ready(nil) == nil {
		t.Fatal("finalizer-only reconciliation initialized an empty store")
	}
	if err := reconcileArtifact(t, reconciler); err == nil {
		t.Fatal("unavailable module unexpectedly reconciled")
	}
	if publication.Ready(nil) == nil {
		t.Fatal("failed reconciliation initialized an empty store")
	}
	unavailable.Store(false)
	if err := reconcileArtifact(t, reconciler); err != nil {
		t.Fatal(err)
	}
	if publication.Ready(nil) != nil || len(reconciler.Store.Snapshot()) != 1 {
		t.Fatal("successful publication did not initialize the authority")
	}
}

func TestStartupAuthorityWaitsForProvisioning(t *testing.T) {
	registry, moduleRef := startupRegistry(t, nil)
	app := artifactApp(moduleRef)
	app.UID = "startup-app"
	app.Spec.KV = true
	scheme := runtime.NewScheme()
	if err := wasmplatformv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := dboperator.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := artifactReconciler(c, registry)
	reconciler.Config.PostgresCredentialNamespace = "default"
	publication := authority.New()
	reconciler.Authority = publication
	publication.Initialize(t.Context(), []wasmplatformv1alpha1.Application{*app})
	result, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(app)})
	if err != nil || result.RequeueAfter == 0 {
		t.Fatalf("expected provisioning retry, got %v, %v", result, err)
	}
	if publication.Ready(nil) == nil {
		t.Fatal("pending provisioning initialized an empty store")
	}
}

type startupInventoryReader struct {
	client.Reader
	firstListed chan struct{}
}

func (r startupInventoryReader) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	err := r.Reader.List(ctx, list, options...)
	select {
	case r.firstListed <- struct{}{}:
	default:
	}
	return err
}

func TestStartupAuthorityRecoversAfterSameNameRecreation(t *testing.T) {
	registry, moduleRef := startupRegistry(t, nil)
	app := artifactApp(moduleRef)
	app.UID = "old-app"
	app.Finalizers = nil
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(app).WithStatusSubresource(app).Build()
	publication := authority.New()
	reconciler := artifactReconciler(c, registry)
	reconciler.Authority = publication
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	listed := make(chan struct{}, 1)
	initializer := authority.Initializer{Authority: publication, Reader: startupInventoryReader{Reader: c, firstListed: listed}}
	if !initializer.NeedLeaderElection() {
		t.Fatal("startup publication can run on a standby")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- initializer.Start(ctx) }()
	select {
	case <-listed:
	case <-ctx.Done():
		t.Fatal("startup inventory was not read")
	}
	if err := c.Delete(ctx, app); err != nil {
		t.Fatal(err)
	}
	replacement := artifactApp(moduleRef)
	replacement.UID = "new-app"
	if err := c.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if err := reconcileArtifact(t, reconciler); err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for publication.Ready(nil) != nil {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("recreated Application left startup authority permanently unavailable")
		}
	}
	if len(reconciler.Store.Snapshot()) != 1 {
		t.Fatal("startup initialized without the replacement configuration")
	}
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("startup authority did not stop")
	}
	if publication.Ready(nil) == nil {
		t.Fatal("stopped leader retained publication authority")
	}
	publication.Reconciled(types.NamespacedName{Namespace: app.Namespace, Name: app.Name}, replacement.UID, replacement.Generation)
	if publication.Ready(nil) == nil {
		t.Fatal("late reconciliation restored lost authority")
	}
}
