package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/artifacts"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/authority"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/controller"
	grpcserver "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/identity"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/routestore"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(wasmplatformv1alpha1.AddToScheme(scheme))
	utilruntime.Must(dboperator.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var grpcPort int
	var webhookCertDir string

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "Address for health probes.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", true, "Enable required configuration publication leader election.")
	flag.IntVar(&grpcPort, "grpc-port", 0, "Port for the gRPC ConfigSync server (overrides GRPC_PORT env).")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/etc/webhook/certs", "Directory containing tls.crt and tls.key for the validating webhook server.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	if !enableLeaderElection {
		ctrl.Log.Error(fmt.Errorf("leader election is required"), "invalid publication configuration")
		os.Exit(1)
	}
	artifactRegistry, err := artifacts.NewRegistry(os.Getenv("APPROVED_REGISTRIES"), os.Getenv("LOCAL_PLAINTEXT_REGISTRIES"))
	if err != nil {
		ctrl.Log.Error(err, "invalid artifact registry configuration")
		os.Exit(1)
	}

	// Resolve gRPC port: flag > env > default.
	if grpcPort == 0 {
		if envPort := os.Getenv("GRPC_PORT"); envPort != "" {
			if _, err := fmt.Sscanf(envPort, "%d", &grpcPort); err != nil || grpcPort == 0 {
				grpcPort = 50051
			}
		} else {
			grpcPort = 50051
		}
	}

	store := configstore.New()
	routeStore := routestore.New()
	publication := authority.New()
	kubernetesConfig := ctrl.GetConfigOrDie()
	authentication, err := authenticationclient.NewForConfig(kubernetesConfig)
	if err != nil {
		ctrl.Log.Error(err, "unable to create workload authentication client")
		os.Exit(1)
	}
	access, err := grpcserver.NewAccess(identity.NewKubernetesReviewer(authentication.TokenReviews()), publication,
		os.Getenv("POD_NAMESPACE"), os.Getenv("EXECUTION_HOST_SERVICE_ACCOUNT"), os.Getenv("GATEWAY_SERVICE_ACCOUNT"))
	if err != nil {
		ctrl.Log.Error(err, "invalid workload identity configuration")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(kubernetesConfig, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "wp-operator.wasm-platform.io",
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			CertDir:  webhookCertDir,
			CertName: "tls.crt",
			KeyName:  "tls.key",
			Port:     9443,
		}),
	})
	if err != nil {
		ctrl.Log.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.ResourceInventoryReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader()}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "ResourceInventory")
		os.Exit(1)
	}
	if err = (&controller.ApplicationReconciler{
		Client:     mgr.GetClient(),
		APIReader:  mgr.GetAPIReader(),
		Scheme:     mgr.GetScheme(),
		Store:      store,
		RouteStore: routeStore,
		Config:     configFromEnv(),
		Artifacts:  artifactRegistry,
		Authority:  publication,
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "Application")
		os.Exit(1)
	}
	if err := mgr.Add(&authority.Initializer{Authority: publication, Reader: mgr.GetAPIReader()}); err != nil {
		ctrl.Log.Error(err, "unable to configure startup publication authority")
		os.Exit(1)
	}

	mgr.GetWebhookServer().Register(
		"/validate-wasm-platform-io-v1alpha1-application",
		&admission.Webhook{
			Handler: &controller.ApplicationValidator{
				Client:  mgr.GetClient(),
				Decoder: admission.NewDecoder(mgr.GetScheme()),
			},
		},
	)

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		ctrl.Log.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("publication", publication.Ready); err != nil {
		ctrl.Log.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	// Start the gRPC ConfigSync server in a background goroutine.
	grpcAddr := fmt.Sprintf(":%d", grpcPort)
	lis, err := net.Listen("tcp", grpcAddr)
	if err != nil {
		ctrl.Log.Error(err, "unable to listen for gRPC", "addr", grpcAddr)
		os.Exit(1)
	}
	grpcSrv := grpc.NewServer(grpc.UnaryInterceptor(access.UnaryInterceptor), grpc.StreamInterceptor(access.StreamInterceptor))
	grpcserver.Register(grpcSrv, store)
	grpcserver.RegisterGateway(grpcSrv, routeStore)
	ctrl.Log.Info("starting gRPC server", "addr", grpcAddr)
	go func() {
		if serveErr := grpcSrv.Serve(lis); serveErr != nil {
			ctrl.Log.Error(serveErr, "gRPC server stopped")
		}
	}()

	ctrl.Log.Info("starting manager")
	managerCtx := ctrl.SetupSignalHandler()
	stopRevocation := context.AfterFunc(managerCtx, publication.Revoke)
	err = mgr.Start(managerCtx)
	stopRevocation()
	publication.Revoke()
	grpcSrv.GracefulStop()
	if err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// configFromEnv builds a controller.Config from well-known environment
// variables. POD_NAMESPACE (injected via the downward API) is used as the
// default namespace for secrets and CRs when a more specific override is absent.
func configFromEnv() controller.Config {
	podNS := os.Getenv("POD_NAMESPACE")

	pgCredNS := os.Getenv("POSTGRES_CREDENTIAL_NAMESPACE")
	if pgCredNS == "" {
		pgCredNS = podNS
	}

	return controller.Config{
		PostgresCredentialNamespace: pgCredNS,
		PostgresVersion:             os.Getenv("POSTGRES_VERSION"),
		PostgresStorageSize:         os.Getenv("POSTGRES_STORAGE_SIZE"),
		RedisStorageSize:            os.Getenv("REDIS_STORAGE_SIZE"),
	}
}
