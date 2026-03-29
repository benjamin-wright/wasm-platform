package controller_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/controller"
)

// ── Test suite bootstrap ──────────────────────────────────────────────────────

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Controller Suite")
}

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	store      *configstore.Store
	testCtx    context.Context
	testCancel context.CancelFunc
)

var testScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(testScheme))
	utilruntime.Must(wasmplatformv1alpha1.AddToScheme(testScheme))
	utilruntime.Must(dboperator.AddToScheme(testScheme))
}

var _ = BeforeSuite(func() {
	ctrl.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))

	// The Application CRD YAML is the raw kubebuilder-generated manifest;
	// it contains no Helm templating so envtest can load it directly.
	crdPath := filepath.Join("..", "..", "helm", "templates")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdPath},
		ErrorIfCRDPathMissing: true,
		BinaryAssetsDirectory: os.Getenv("KUBEBUILDER_ASSETS"),
	}

	cfg, err := testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	Expect(err).NotTo(HaveOccurred())

	store = configstore.New()

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 testScheme,
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	Expect(err).NotTo(HaveOccurred())

	err = (&controller.ApplicationReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Store:  store,
		Config: controller.Config{
			PostgresDatabaseName:        "test-postgres",
			PostgresCredentialNamespace: "default",
			RedisSecretName:             "test-redis-credentials",
			RedisSecretNamespace:        "default",
		},
	}).SetupWithManager(mgr)
	Expect(err).NotTo(HaveOccurred())

	testCtx, testCancel = context.WithCancel(context.Background())
	go func() {
		defer GinkgoRecover()
		Expect(mgr.Start(testCtx)).To(Succeed())
	}()
})

var _ = AfterSuite(func() {
	testCancel()
	Expect(testEnv.Stop()).To(Succeed())
})

// ── Helpers ───────────────────────────────────────────────────────────────────

func makeApp(name, namespace string) *wasmplatformv1alpha1.Application {
	return &wasmplatformv1alpha1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: wasmplatformv1alpha1.ApplicationSpec{
			Module: "oci://registry.example.com/hello@sha256:abc123",
			Topic:  "fn.hello",
		},
	}
}

func storeContains(ns, name string) func() bool {
	return func() bool {
		for _, cfg := range store.Snapshot() {
			if cfg.Namespace == ns && cfg.Name == name {
				return true
			}
		}
		return false
	}
}

func storeAbsent(ns, name string) func() bool {
	return func() bool {
		return !storeContains(ns, name)()
	}
}

// ── Specs ─────────────────────────────────────────────────────────────────────

var _ = Describe("ApplicationReconciler", func() {
	const ns = "default"
	const timeout = 10 * time.Second
	const interval = 250 * time.Millisecond

	Describe("Create", func() {
		It("adds the Application to the config store", func() {
			app := makeApp("create-test", ns)
			Expect(k8sClient.Create(testCtx, app)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(testCtx, app) })

			Eventually(storeContains(ns, app.Name), timeout, interval).Should(BeTrue())
		})

		It("sets the Ready condition to True", func() {
			app := makeApp("ready-condition-test", ns)
			Expect(k8sClient.Create(testCtx, app)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(testCtx, app) })

			Eventually(func() string {
				var fetched wasmplatformv1alpha1.Application
				if err := k8sClient.Get(testCtx, types.NamespacedName{Name: app.Name, Namespace: ns}, &fetched); err != nil {
					return ""
				}
				for _, c := range fetched.Status.Conditions {
					if c.Type == "Ready" {
						return string(c.Status)
					}
				}
				return ""
			}, timeout, interval).Should(Equal("True"))
		})
	})

	Describe("Update", func() {
		It("propagates spec changes to the config store", func() {
			app := makeApp("update-test", ns)
			Expect(k8sClient.Create(testCtx, app)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(testCtx, app) })

			Eventually(storeContains(ns, app.Name), timeout, interval).Should(BeTrue())

			// Retrieve the latest resource version before updating.
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: app.Name, Namespace: ns}, app)).To(Succeed())
			app.Spec.Topic = "fn.updated"
			Expect(k8sClient.Update(testCtx, app)).To(Succeed())

			Eventually(func() string {
				for _, cfg := range store.Snapshot() {
					if cfg.Namespace == ns && cfg.Name == app.Name {
						return cfg.Topic
					}
				}
				return ""
			}, timeout, interval).Should(Equal("fn.updated"))
		})
	})

	Describe("Delete", func() {
		It("removes the Application from the config store", func() {
			app := makeApp("delete-test", ns)
			Expect(k8sClient.Create(testCtx, app)).To(Succeed())

			Eventually(storeContains(ns, app.Name), timeout, interval).Should(BeTrue())

			Expect(k8sClient.Delete(testCtx, app)).To(Succeed())
			Eventually(storeAbsent(ns, app.Name), timeout, interval).Should(BeTrue())
		})
	})

	Describe("KeyValue binding", func() {
		It("populates KeyValueConfig when spec.keyValue is set and the Redis Secret exists", func() {
			// Pre-create the Redis credentials Secret the reconciler reads.
			redisSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-redis-credentials",
					Namespace: ns,
				},
				Data: map[string][]byte{
					"REDIS_USERNAME": []byte("wp-operator"),
					"REDIS_PASSWORD": []byte("s3cr3t"),
					"REDIS_HOST":     []byte("redis.default.svc.cluster.local"),
					"REDIS_PORT":     []byte("6379"),
				},
			}
			Expect(k8sClient.Create(testCtx, redisSecret)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(testCtx, redisSecret) })

			app := makeApp("kv-test", ns)
			app.Spec.KeyValue = "sessions"
			Expect(k8sClient.Create(testCtx, app)).To(Succeed())
			DeferCleanup(func() { _ = k8sClient.Delete(testCtx, app) })

			Eventually(func() string {
				for _, cfg := range store.Snapshot() {
					if cfg.Namespace == ns && cfg.Name == app.Name && cfg.KeyValue != nil {
						return cfg.KeyValue.Prefix
					}
				}
				return ""
			}, timeout, interval).Should(Equal("default/sessions/"))
		})
	})
})
