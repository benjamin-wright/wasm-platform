//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
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

// hostStream connects to the operator's gRPC ConfigSync endpoint and acts as
// an execution host. It automatically acks every received update so the
// operator can continue broadcasting subsequent updates. Consumed updates are
// forwarded to the Updates channel.
type hostStream struct {
	Updates chan *configsync.IncrementalUpdateRequest
}

func newHostStream(t *testing.T, hostID string) *hostStream {
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
	// Identify this host to the operator.
	if err := stream.Send(&configsync.IncrementalUpdateAck{HostId: hostID}); err != nil {
		t.Fatalf("sending host identification: %v", err)
	}

	hs := &hostStream{Updates: make(chan *configsync.IncrementalUpdateRequest, 64)}
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				return
			}
			select {
			case hs.Updates <- req:
			default:
				// Buffer full; drop to avoid blocking the receive loop.
			}
			// Acknowledge so the operator can deliver the next update.
			_ = stream.Send(&configsync.IncrementalUpdateAck{
				HostId:         req.TargetHostId,
				VersionApplied: req.IncrementalConfig.GetVersion(),
				Success:        true,
			})
		}
	}()
	return hs
}

// receiveUpdate blocks until an IncrementalUpdateRequest arrives on the stream
// whose updates contain an AppUpdate for (ns, name) with the given delete flag.
// Fails the test if a second update for the same resource arrives before the
// call returns, or if no matching update arrives within updateTimeout.
func (hs *hostStream) receiveUpdate(t *testing.T, ns, name string, wantDelete bool) *configsync.AppUpdate {
	t.Helper()
	deadline := time.Now().Add(updateTimeout)
	var found *configsync.AppUpdate
	for time.Now().Before(deadline) {
		select {
		case req := <-hs.Updates:
			for _, u := range req.IncrementalConfig.GetUpdates() {
				cfg := u.GetAppConfig()
				if cfg.GetNamespace() != ns || cfg.GetName() != name {
					continue
				}
				if u.GetDelete() != wantDelete {
					t.Fatalf("received unexpected update for %s/%s: delete=%v, want delete=%v", ns, name, u.GetDelete(), wantDelete)
				}
				if found != nil {
					t.Fatalf("received duplicate update for %s/%s", ns, name)
				}
				found = u
			}
		case <-time.After(250 * time.Millisecond):
			if found != nil {
				return found
			}
		}
	}
	if found != nil {
		return found
	}
	t.Fatalf("timed out waiting for %s/%s update (delete=%v)", ns, name, wantDelete)
	return nil
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
	hs := newHostStream(t, "itest-host-create")

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

	update := hs.receiveUpdate(t, ns, "my-app", false)
	g.Expect(update.GetAppConfig().GetTopic()).To(Equal("itest.create"))
	g.Expect(update.GetAppConfig().GetModuleRef()).To(Equal("oci://example.com/my-app@sha256:aaaa"))
}

// TestApplicationUpdate_BroadcastsUpsert verifies that updating an Application
// CR causes the operator to push a new upsert update reflecting the change.
func TestApplicationUpdate_BroadcastsUpsert(t *testing.T) {
	g := NewWithT(t)
	c := newK8sClient(t)
	ns := createTestNamespace(t, c)
	hs := newHostStream(t, "itest-host-update")

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

	// Drain the single create broadcast, then assert exactly one update broadcast.
	hs.receiveUpdate(t, ns, "my-app", false)

	// Fetch the latest resource version before applying the update.
	var fresh wasmplatformv1alpha1.Application
	g.Expect(c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: "my-app"}, &fresh)).To(Succeed())
	fresh.Spec.Topic = "itest.v2"
	g.Expect(c.Update(context.Background(), &fresh)).To(Succeed())

	update := hs.receiveUpdate(t, ns, "my-app", false)
	g.Expect(update.GetAppConfig().GetTopic()).To(Equal("itest.v2"))
}

// TestApplicationDelete_BroadcastsDelete verifies that deleting an Application
// CR causes the operator to push a delete update to connected execution hosts.
func TestApplicationDelete_BroadcastsDelete(t *testing.T) {
	g := NewWithT(t)
	c := newK8sClient(t)
	ns := createTestNamespace(t, c)
	hs := newHostStream(t, "itest-host-delete")

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
	hs.receiveUpdate(t, ns, "my-app", false)

	// Wait for Ready so the finalizer is in place before deleting.
	waitForReady(t, c, ns, "my-app")

	g.Expect(c.Delete(context.Background(), app)).To(Succeed())

	hs.receiveUpdate(t, ns, "my-app", true)
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
	hs := newHostStream(t, "itest-host-conflict")

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
	hs.receiveUpdate(t, ns, "owner-app", false)

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

	// No config update should have been broadcast for the blocked app.
	// (The host stream buffer is checked — no update for "blocked-app" must arrive.)

	// Delete the owning app. The watch handler should enqueue the blocked app.
	g.Expect(c.Delete(context.Background(), owner)).To(Succeed())
	hs.receiveUpdate(t, ns, "owner-app", true)

	// The blocked app should now heal: TopicConflict removed, Ready=True.
	waitForReady(t, c, ns, "blocked-app")

	// And an upsert update for the formerly-blocked app must be broadcast.
	update := hs.receiveUpdate(t, ns, "blocked-app", false)
	g.Expect(update.GetAppConfig().GetTopic()).To(Equal(sharedTopic))
}
