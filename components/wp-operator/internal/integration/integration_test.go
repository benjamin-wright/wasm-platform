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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
)

const updateTimeout = 30 * time.Second

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
	mu      sync.Mutex
	upserts map[string]chan *configsync.ApplicationConfig // keyed by "namespace/name"
	deletes map[string]chan struct{}                      // keyed by "namespace/name"
}

func newConfigClient(t *testing.T, hostID string) *configClient {
	t.Helper()
	conn, err := grpc.NewClient(grpcAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("connecting to operator gRPC at %s: %v", grpcAddr(), err)
	}

	ctx, cancel := context.WithCancel(context.Background())
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
	}
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
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
			_ = stream.Send(&configsync.IncrementalUpdateAck{
				HostId:         req.TargetHostId,
				VersionApplied: req.IncrementalConfig.GetVersion(),
				Success:        true,
			})
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
	case <-time.After(updateTimeout):
		t.Fatalf("timed out waiting for delete of %s/%s", ns, name)
	}
}

// waitForReady polls the Application until its Ready condition is True.
func waitForReady(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	g := NewWithT(t)
	g.Eventually(func() bool {
		var app wasmplatformv1alpha1.Application
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &app); err != nil {
			return false
		}
		for _, cond := range app.Status.Conditions {
			if cond.Type == "Ready" && cond.Status == "True" {
				return true
			}
		}
		return false
	}, updateTimeout, 500*time.Millisecond).Should(BeTrue(), "application %s/%s should become Ready", ns, name)
}

// TestApplicationCreate_BroadcastsUpsert verifies that creating an Application
// CR causes the operator to push an upsert update to connected execution hosts.
func TestApplicationCreate_BroadcastsUpsert(t *testing.T) {
	g := NewWithT(t)
	c := newK8sClient(t)
	ns := createTestNamespace(t, c)
	cc := newConfigClient(t, "itest-host-create")

	app := &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: ns},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Module: "oci://example.com/my-app@sha256:aaaa",
			Topic:  "itest.create",
		},
	}
	g.Expect(c.Create(context.Background(), app)).To(Succeed())
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), app)
	})

	update := cc.WaitForUpsert(t, ns, "my-app")
	g.Expect(update.GetTopic()).To(Equal("itest.create"))
	g.Expect(update.GetModuleRef()).To(Equal("oci://example.com/my-app@sha256:aaaa"))
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
			Module: "oci://example.com/my-app@sha256:bbbb",
			Topic:  "itest.v1",
		},
	}
	g.Expect(c.Create(context.Background(), app)).To(Succeed())
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), app)
	})

	// Drain the create broadcast before asserting the update broadcast.
	cc.WaitForUpsert(t, ns, "my-app")

	// Fetch the latest resource version before applying the update.
	var fresh wasmplatformv1alpha1.Application
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "my-app"}, &fresh)).To(Succeed())
	fresh.Spec.Topic = "itest.v2"
	g.Expect(c.Update(context.Background(), &fresh)).To(Succeed())

	update := cc.WaitForUpsert(t, ns, "my-app")
	g.Expect(update.GetTopic()).To(Equal("itest.v2"))
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
			Module: "oci://example.com/my-app@sha256:cccc",
			Topic:  "itest.delete",
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

// waitForCondition polls the Application until the named condition reaches the
// expected True/False status.
func waitForCondition(t *testing.T, c client.Client, ns, name, condType string, wantTrue bool) {
	t.Helper()
	g := NewWithT(t)
	g.Eventually(func() bool {
		var app wasmplatformv1alpha1.Application
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &app); err != nil {
			return false
		}
		for _, cond := range app.Status.Conditions {
			if cond.Type == condType {
				return (cond.Status == "True") == wantTrue
			}
		}
		return false
	}, updateTimeout, 500*time.Millisecond).Should(BeTrue(),
		"application %s/%s condition %s should have status %v", ns, name, condType, wantTrue)
}

// TestTopicConflict_BlockedAppHealsOnOwnerDelete verifies that:
//   - When two Applications claim the same topic, the newer one is blocked with
//     TopicConflict and Ready=False.
//   - When the owning app is deleted, the blocked app wakes up, reconciles
//     successfully, and becomes Ready=True.
func TestTopicConflict_BlockedAppHealsOnOwnerDelete(t *testing.T) {
	g := NewWithT(t)
	c := newK8sClient(t)
	ns := createTestNamespace(t, c)
	cc := newConfigClient(t, "itest-host-conflict")

	const sharedTopic = "itest.conflict.heal"

	// Create the owner first so it has the older creationTimestamp.
	owner := &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "owner-app", Namespace: ns},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Module: "oci://example.com/owner@sha256:1111",
			Topic:  sharedTopic,
		},
	}
	g.Expect(c.Create(context.Background(), owner)).To(Succeed())
	t.Cleanup(func() { _ = c.Delete(context.Background(), owner) })

	// Owner must be Ready before we create the blocker; this also ensures the
	// creationTimestamp ordering is deterministic.
	waitForReady(t, c, ns, "owner-app")
	cc.WaitForUpsert(t, ns, "owner-app")

	// Sleep for a full second so that blocked-app receives a strictly later
	// creationTimestamp. Kubernetes stores creationTimestamp at second
	// granularity, so without this sleep both apps may land on the same
	// second and the lexicographic tie-break ("blocked" < "owner") would
	// incorrectly make blocked-app the topic owner.
	time.Sleep(time.Second)

	// Create the blocked app — it should pick up TopicConflict immediately.
	blocked := &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{Name: "blocked-app", Namespace: ns},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Module: "oci://example.com/blocked@sha256:2222",
			Topic:  sharedTopic,
		},
	}

	g.Expect(c.Create(context.Background(), blocked)).To(Succeed())
	t.Cleanup(func() { _ = c.Delete(context.Background(), blocked) })

	// Blocked app must reach TopicConflict=True and Ready=False.
	waitForCondition(t, c, ns, "blocked-app", "TopicConflict", true)
	waitForCondition(t, c, ns, "blocked-app", "Ready", false)

	// Delete the owning app. The watch handler should enqueue the blocked app.
	g.Expect(c.Delete(context.Background(), owner)).To(Succeed())
	cc.WaitForDelete(t, ns, "owner-app")

	// The blocked app should now heal: TopicConflict removed, Ready=True.
	waitForReady(t, c, ns, "blocked-app")

	// An upsert for the formerly-blocked app must be broadcast.
	update := cc.WaitForUpsert(t, ns, "blocked-app")
	g.Expect(update.GetTopic()).To(Equal(sharedTopic))
}
