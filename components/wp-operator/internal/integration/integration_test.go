//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
)

const (
	requestTimeout = 5 * time.Second
	updateTimeout  = 30 * time.Second
	streamTimeout  = 5 * time.Minute
)

var scheme *runtime.Scheme

func init() {
	scheme = runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(wasmplatformv1alpha1.AddToScheme(scheme))
}

// grpcAddr returns the operator gRPC endpoint, defaulting to the forwarded port.
func grpcAddr() string {
	if addr := os.Getenv("GRPC_ADDR"); addr != "" {
		return addr
	}
	return "localhost:50051"
}

// newK8sClient builds a controller-runtime client from KUBECONFIG.
func newK8sClient(t *testing.T) client.Client {
	t.Helper()
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		t.Fatal("KUBECONFIG env var is not set; run 'export KUBECONFIG=~/.scratch/wasm-platform.yaml' first")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		t.Fatalf("building kubeconfig: %v", err)
	}
	cfg.Timeout = requestTimeout
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("creating k8s client: %v", err)
	}
	return c
}

// createTestNamespace creates a randomly-named namespace and registers a
// cleanup that deletes it when the test ends.
func createTestNamespace(t *testing.T, c client.Client) string {
	t.Helper()
	name := fmt.Sprintf("wp-itest-%d", rand.Intn(900000)+100000)
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	if err := c.Create(context.Background(), ns); err != nil {
		t.Fatalf("creating namespace %s: %v", name, err)
	}
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: name},
		})
	})
	return name
}

// configClient connects to the operator gRPC endpoint, identifies itself as
// an execution host, and collects every incoming AppUpdate in memory. Updates
// are routed into per-resource buffered channels so that WaitForUpsert and
// WaitForDelete calls for different resources are fully independent.
type configClient struct {
	mu        sync.Mutex
	upserts   map[string]chan *configsync.ApplicationConfig // keyed by "namespace/name"
	deletes   map[string]chan struct{}                      // keyed by "namespace/name"
	done      chan struct{}
	streamErr error
}

func newConfigClient(t *testing.T, hostID string) *configClient {
	t.Helper()
	conn, err := grpc.NewClient(grpcAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("connecting to operator gRPC at %s: %v", grpcAddr(), err)
	}

	deadline := time.Now().Add(streamTimeout)
	if testDeadline, ok := t.Deadline(); ok && testDeadline.Before(deadline) {
		deadline = testDeadline
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline)
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
	})

	stream, err := configsync.NewConfigSyncClient(conn).PushIncrementalUpdate(ctx)
	if err != nil {
		t.Fatalf("opening PushIncrementalUpdate stream: %v", err)
	}
	if err := stream.Send(&configsync.IncrementalUpdateAck{HostId: hostID}); err != nil {
		t.Fatalf("sending host identification: %v", err)
	}

	cc := &configClient{
		upserts: make(map[string]chan *configsync.ApplicationConfig),
		deletes: make(map[string]chan struct{}),
		done:    make(chan struct{}),
	}
	go func() {
		defer close(cc.done)
		for {
			req, err := stream.Recv()
			if err != nil {
				cc.streamErr = fmt.Errorf("receiving configuration update: %w", err)
				return
			}
			for _, u := range req.IncrementalConfig.GetUpdates() {
				cfg := u.GetAppConfig()
				key := cfg.GetNamespace() + "/" + cfg.GetName()
				if u.GetDelete() {
					select {
					case cc.deleteChan(key) <- struct{}{}:
					default:
					}
				} else {
					select {
					case cc.upsertChan(key) <- cfg:
					default:
					}
				}
			}
			// Acknowledge so the operator can deliver the next update.
			if err := stream.Send(&configsync.IncrementalUpdateAck{
				HostId:         req.TargetHostId,
				VersionApplied: req.IncrementalConfig.GetVersion(),
				Success:        true,
			}); err != nil {
				cc.streamErr = fmt.Errorf("acknowledging configuration update: %w", err)
				return
			}
		}
	}()
	return cc
}

func (cc *configClient) upsertChan(key string) chan *configsync.ApplicationConfig {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if ch, ok := cc.upserts[key]; ok {
		return ch
	}
	ch := make(chan *configsync.ApplicationConfig, 32)
	cc.upserts[key] = ch
	return ch
}

func (cc *configClient) deleteChan(key string) chan struct{} {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	if ch, ok := cc.deletes[key]; ok {
		return ch
	}
	ch := make(chan struct{}, 8)
	cc.deletes[key] = ch
	return ch
}

// WaitForUpsert blocks until an upsert for (ns, name) has been received and
// returns its ApplicationConfig. Fails the test if none arrives within updateTimeout.
func (cc *configClient) WaitForUpsert(t *testing.T, ns, name string) *configsync.ApplicationConfig {
	t.Helper()
	select {
	case cfg := <-cc.upsertChan(ns + "/" + name):
		return cfg
	case <-cc.done:
		t.Fatalf("configuration stream ended while waiting for upsert of %s/%s: %v", ns, name, cc.streamErr)
		return nil
	case <-time.After(updateTimeout):
		t.Fatalf("timed out waiting for upsert of %s/%s", ns, name)
		return nil
	}
}

// WaitForDelete blocks until a delete for (ns, name) has been received.
// Fails the test if none arrives within updateTimeout.
func (cc *configClient) WaitForDelete(t *testing.T, ns, name string) {
	t.Helper()
	select {
	case <-cc.deleteChan(ns + "/" + name):
	case <-cc.done:
		t.Fatalf("configuration stream ended while waiting for delete of %s/%s: %v", ns, name, cc.streamErr)
	case <-time.After(updateTimeout):
		t.Fatalf("timed out waiting for delete of %s/%s", ns, name)
	}
}

// waitForReady polls the Application until its Ready condition is True.
func waitForReady(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	g := NewWithT(t)
	ctx, cancel := context.WithTimeout(t.Context(), updateTimeout)
	defer cancel()
	g.Eventually(func() bool {
		var app wasmplatformv1alpha1.Application
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &app); err != nil {
			return false
		}
		for _, cond := range app.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				return true
			}
		}
		return false
	}, updateTimeout, 500*time.Millisecond).WithContext(ctx).Should(BeTrue(), "application %s/%s should become Ready", ns, name)
}

func fixtureModuleRef(t *testing.T, c client.Client) string {
	t.Helper()
	var moduleRef string
	ctx, cancel := context.WithTimeout(t.Context(), updateTimeout)
	defer cancel()
	NewWithT(t).Eventually(func() bool {
		var app wasmplatformv1alpha1.Application
		if err := c.Get(ctx, types.NamespacedName{Namespace: "examples", Name: "demo-app"}, &app); err != nil {
			return false
		}
		for _, module := range app.Status.ResolvedModules {
			if module.Name == "message-handler" && module.Generation == app.Generation {
				moduleRef = module.ModuleRef
				return true
			}
		}
		return false
	}, updateTimeout, 500*time.Millisecond).WithContext(ctx).Should(BeTrue(), "demo-app should have a resolved message module")
	return moduleRef
}

// TestApplicationCreate_BroadcastsUpsert verifies that creating an Application
// CR causes the operator to push an upsert update to connected execution hosts.
func TestApplicationCreate_BroadcastsUpsert(t *testing.T) {
	g := NewWithT(t)
	c := newK8sClient(t)
	ns := createTestNamespace(t, c)
	cc := newConfigClient(t, "itest-host-create")
	moduleRef := fixtureModuleRef(t, c)

	app := &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: ns},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Functions: []wasmplatformv1alpha1.FunctionSpec{
				{
					Name:   "handler",
					Module: moduleRef,
					Trigger: wasmplatformv1alpha1.FunctionTrigger{
						Topic: "itest.create",
					},
				},
			},
		},
	}
	g.Expect(c.Create(context.Background(), app)).To(Succeed())
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), app)
	})

	update := cc.WaitForUpsert(t, ns, "my-app")
	g.Expect(update.GetFunctions()[0].GetTopic()).To(Equal("fn.itest.create"))
	g.Expect(update.GetFunctions()[0].GetModuleRef()).To(Equal(moduleRef))
}

// TestApplicationUpdate_BroadcastsUpsert verifies that updating an Application
// CR causes the operator to push a new upsert update reflecting the change.
func TestApplicationUpdate_BroadcastsUpsert(t *testing.T) {
	g := NewWithT(t)
	c := newK8sClient(t)
	ns := createTestNamespace(t, c)
	cc := newConfigClient(t, "itest-host-update")

	app := &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: ns},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Functions: []wasmplatformv1alpha1.FunctionSpec{
				{
					Name:   "handler",
					Module: fixtureModuleRef(t, c),
					Trigger: wasmplatformv1alpha1.FunctionTrigger{
						Topic: "itest.v1",
					},
				},
			},
		},
	}
	g.Expect(c.Create(context.Background(), app)).To(Succeed())
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), app)
	})

	// Drain the create broadcast before asserting the update broadcast.
	cc.WaitForUpsert(t, ns, "my-app")

	g.Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var fresh wasmplatformv1alpha1.Application
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "my-app"}, &fresh); err != nil {
			return err
		}
		fresh.Spec.Functions[0].Trigger.Topic = "itest.v2"
		return c.Update(context.Background(), &fresh)
	})).To(Succeed())

	update := cc.WaitForUpsert(t, ns, "my-app")
	g.Expect(update.GetFunctions()[0].GetTopic()).To(Equal("fn.itest.v2"))
}

// TestApplicationDelete_BroadcastsDelete verifies that deleting an Application
// CR causes the operator to push a delete update to connected execution hosts.
func TestApplicationDelete_BroadcastsDelete(t *testing.T) {
	g := NewWithT(t)
	c := newK8sClient(t)
	ns := createTestNamespace(t, c)
	cc := newConfigClient(t, "itest-host-delete")

	app := &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: ns},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Functions: []wasmplatformv1alpha1.FunctionSpec{
				{
					Name:   "handler",
					Module: fixtureModuleRef(t, c),
					Trigger: wasmplatformv1alpha1.FunctionTrigger{
						Topic: "itest.delete",
					},
				},
			},
		},
	}
	g.Expect(c.Create(context.Background(), app)).To(Succeed())
	t.Cleanup(func() {
		// Ensure the CR is removed even if the test fails before the delete step.
		_ = c.Delete(context.Background(), app)
	})

	// Drain the create upsert before proceeding to the delete assertion.
	cc.WaitForUpsert(t, ns, "my-app")

	// Wait for Ready so the finalizer is in place before deleting.
	waitForReady(t, c, ns, "my-app")

	g.Expect(c.Delete(context.Background(), app)).To(Succeed())

	cc.WaitForDelete(t, ns, "my-app")
}

func TestTopicConflict_RejectedUntilOwnerDeleted(t *testing.T) {
	g := NewWithT(t)
	c := newK8sClient(t)
	ns := createTestNamespace(t, c)
	cc := newConfigClient(t, "itest-host-conflict")

	const sharedTopic = "itest.conflict.heal"

	owner := &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "owner-app", Namespace: ns},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Functions: []wasmplatformv1alpha1.FunctionSpec{
				{
					Name:   "handler",
					Module: fixtureModuleRef(t, c),
					Trigger: wasmplatformv1alpha1.FunctionTrigger{
						Topic: sharedTopic,
					},
				},
			},
		},
	}
	g.Expect(c.Create(context.Background(), owner)).To(Succeed())
	t.Cleanup(func() { _ = c.Delete(context.Background(), owner) })

	waitForReady(t, c, ns, "owner-app")
	cc.WaitForUpsert(t, ns, "owner-app")

	blocked := &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "blocked-app", Namespace: ns},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Functions: []wasmplatformv1alpha1.FunctionSpec{
				{
					Name:   "handler",
					Module: fixtureModuleRef(t, c),
					Trigger: wasmplatformv1alpha1.FunctionTrigger{
						Topic: sharedTopic,
					},
				},
			},
		},
	}

	t.Cleanup(func() { _ = c.Delete(context.Background(), blocked) })
	err := c.Create(context.Background(), blocked.DeepCopy())
	g.Expect(apierrors.IsForbidden(err)).To(BeTrue(), "expected admission rejection, got %v", err)
	g.Expect(err).To(MatchError(ContainSubstring(fmt.Sprintf("topic %q is already claimed by %s/owner-app", sharedTopic, ns))))

	var rejected wasmplatformv1alpha1.Application
	err = c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "blocked-app"}, &rejected)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "rejected application must not be persisted, got %v", err)

	g.Expect(c.Delete(context.Background(), owner)).To(Succeed())
	cc.WaitForDelete(t, ns, "owner-app")

	// Admission uses the informer cache, which may briefly retain the deleted owner.
	ctx, cancel := context.WithTimeout(t.Context(), updateTimeout)
	defer cancel()
	g.Eventually(func() error {
		return c.Create(ctx, blocked.DeepCopy())
	}, updateTimeout, 500*time.Millisecond).WithContext(ctx).Should(Succeed())
	waitForReady(t, c, ns, "blocked-app")

	update := cc.WaitForUpsert(t, ns, "blocked-app")
	g.Expect(update.GetFunctions()[0].GetTopic()).To(Equal("fn." + sharedTopic))
}
