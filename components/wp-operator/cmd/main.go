package main

import (
	"flag"
	"fmt"
	"net"
	"os"

	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	dboperator "github.com/benjamin-wright/db-operator/pkg/api/v1alpha1"
	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/controller"
	grpcserver "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc"
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
	flag.BoolVar(&enableLeaderElection, "leader-elect", false, "Enable leader election for high availability.")
	flag.IntVar(&grpcPort, "grpc-port", 0, "Port for the gRPC ConfigSync server (overrides GRPC_PORT env).")
	flag.StringVar(&webhookCertDir, "webhook-cert-dir", "/etc/webhook/certs", "Directory containing tls.crt and tls.key for the validating webhook server.")

	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

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

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
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

	if err = (&controller.ApplicationReconciler{
		Client:     mgr.GetClient(),
		Scheme:     mgr.GetScheme(),
		Store:      store,
		RouteStore: routeStore,
		Config:     configFromEnv(),
	}).SetupWithManager(mgr); err != nil {
		ctrl.Log.Error(err, "unable to create controller", "controller", "Application")
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
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
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
	grpcSrv := grpc.NewServer()
	grpcserver.Register(grpcSrv, store)
	grpcserver.RegisterGateway(grpcSrv, routeStore)
	ctrl.Log.Info("starting gRPC server", "addr", grpcAddr)
	go func() {
		if serveErr := grpcSrv.Serve(lis); serveErr != nil {
			ctrl.Log.Error(serveErr, "gRPC server stopped")
		}
	}()

	ctrl.Log.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		ctrl.Log.Error(err, "problem running manager")
		os.Exit(1)
	}

	grpcSrv.GracefulStop()
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
		PostgresDatabaseName:        os.Getenv("POSTGRES_DATABASE_NAME"),
		PostgresCredentialNamespace: pgCredNS,
	}
}
