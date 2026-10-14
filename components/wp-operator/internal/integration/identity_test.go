//go:build integration

package integration_test

import (
	"context"
	"os"
	"testing"
	"time"

	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	gateway "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/gateway"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func testToken(t *testing.T, serviceAccount, audience string) string {
	t.Helper()
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatal("could not load Kubernetes configuration")
	}
	config.Timeout = requestTimeout
	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal("could not create Kubernetes client")
	}
	ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
	defer cancel()
	expiration := int64(600)
	result, err := client.CoreV1().ServiceAccounts("wasm-platform").CreateToken(ctx, serviceAccount, &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: &expiration},
	}, metav1.CreateOptions{})
	if err != nil || result.Status.Token == "" {
		t.Fatal("could not issue test workload token")
	}
	return result.Status.Token
}

func TestConfigurationIdentity(t *testing.T) {
	conn, err := grpc.NewClient(grpcAddr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal("could not create configuration client")
	}
	t.Cleanup(func() { _ = conn.Close() })
	host := configsync.NewConfigSyncClient(conn)
	routes := gateway.NewGatewayRoutesClient(conn)
	hostToken := testToken(t, "execution-host", "wasm-platform")
	gatewayToken := testToken(t, "gateway", "wasm-platform")
	for _, test := range []struct {
		name, token string
		code        codes.Code
	}{
		{"anonymous", "", codes.Unauthenticated},
		{"invalid", "invalid-workload-token", codes.Unauthenticated},
		{"wrong-audience", testToken(t, "execution-host", "wrong-audience"), codes.Unauthenticated},
		{"wrong-service-account", testToken(t, "default", "wasm-platform"), codes.PermissionDenied},
		{"gateway-cannot-read-credentials", gatewayToken, codes.PermissionDenied},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := identityContext(t, test.token)
			defer cancel()
			response, err := host.RequestFullConfig(ctx, &configsync.FullConfigRequest{HostId: "execution-host"})
			if status.Code(err) != test.code || response != nil {
				t.Fatalf("configuration rejection: got %s, want %s; credentials must not be returned", status.Code(err), test.code)
			}
		})
	}
	t.Run("host-snapshot", func(t *testing.T) {
		ctx, cancel := identityContext(t, hostToken)
		defer cancel()
		response, err := host.RequestFullConfig(ctx, &configsync.FullConfigRequest{HostId: "identity-regression"})
		if err != nil || !response.GetSuccess() || len(response.GetConfig().GetApplications()) == 0 {
			t.Fatal("authorized execution host did not receive initialized configuration")
		}
	})
	t.Run("gateway-routes", func(t *testing.T) {
		ctx, cancel := identityContext(t, gatewayToken)
		defer cancel()
		response, err := routes.RequestFullRoutes(ctx, &gateway.FullRoutesRequest{GatewayId: "identity-regression"})
		if err != nil || !response.GetSuccess() || len(response.GetRoutes()) == 0 {
			t.Fatal("authorized gateway did not receive initialized routes")
		}
	})
	t.Run("host-cannot-claim-gateway", func(t *testing.T) {
		ctx, cancel := identityContext(t, hostToken)
		defer cancel()
		response, err := routes.RequestFullRoutes(ctx, &gateway.FullRoutesRequest{GatewayId: "gateway"})
		if status.Code(err) != codes.PermissionDenied || response != nil {
			t.Fatal("execution-host token was accepted for gateway routes")
		}
	})
}

func identityContext(t *testing.T, token string) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	if token != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	}
	return ctx, cancel
}
