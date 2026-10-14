package grpcserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	wasmplatformv1alpha1 "github.com/benjamin-wright/wasm-platform/wp-operator/api/v1alpha1"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/authority"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	gateway "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/gateway"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/identity"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/routestore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

type tokenReviewFunc func(context.Context, *authenticationv1.TokenReview, metav1.CreateOptions) (*authenticationv1.TokenReview, error)

func (f tokenReviewFunc) Create(ctx context.Context, review *authenticationv1.TokenReview, options metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
	return f(ctx, review, options)
}

func projectedToken(role string, expiry time.Time) string {
	claims := fmt.Sprintf(`{"exp":%d,"sub":%q}`, expiry.Unix(), role)
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
}

func newTestAccess(t *testing.T, publication *authority.Authority, reviewer identity.Reviewer) *Access {
	t.Helper()
	access, err := NewAccess(reviewer, publication, "platform", "execution-host", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	return access
}

func workloadReviewer(t *testing.T, hostToken, gatewayToken string, revoked *atomic.Bool) identity.Reviewer {
	t.Helper()
	return identity.NewKubernetesReviewer(tokenReviewFunc(func(ctx context.Context, request *authenticationv1.TokenReview, _ metav1.CreateOptions) (*authenticationv1.TokenReview, error) {
		if len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != identity.Audience {
			t.Error("TokenReview did not request the platform audience")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Error("TokenReview did not have its finite deadline")
		}
		role := ""
		switch request.Spec.Token {
		case hostToken:
			role = "execution-host"
		case gatewayToken:
			role = "gateway"
		}
		return &authenticationv1.TokenReview{Status: authenticationv1.TokenReviewStatus{
			Authenticated: role != "" && (revoked == nil || !revoked.Load()), Audiences: []string{identity.Audience},
			User: authenticationv1.UserInfo{Username: "system:serviceaccount:platform:" + role},
		}}, nil
	}))
}

func protectedEndpoint(t *testing.T, access *Access, configs *configstore.Store, routes *routestore.Store) (context.Context, *grpc.ClientConn) {
	t.Helper()
	ctx, conn, _ := newLoggedGRPCServer(t, func(server *grpc.Server) {
		Register(server, configs)
		RegisterGateway(server, routes)
	}, grpc.UnaryInterceptor(access.UnaryInterceptor), grpc.StreamInterceptor(access.StreamInterceptor))
	return ctx, conn
}

func bearer(ctx context.Context, token string) context.Context {
	return metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+token))
}

func assertCode(t *testing.T, err error, expected codes.Code) {
	t.Helper()
	if status.Code(err) != expected {
		t.Fatalf("got %v, expected %s", err, expected)
	}
}

func TestSnapshotsRequireRoleAndInitializedLeader(t *testing.T) {
	publication := authority.New()
	hostToken := projectedToken("host", time.Now().Add(time.Hour))
	gatewayToken := projectedToken("gateway", time.Now().Add(time.Hour))
	access := newTestAccess(t, publication, workloadReviewer(t, hostToken, gatewayToken, nil))
	configs, routes := configstore.New(), routestore.New()
	key := types.NamespacedName{Namespace: "apps", Name: "demo"}
	configs.Set(key, &configsync.ApplicationConfig{Name: "demo", Env: map[string]string{"PASSWORD": "private-credential"}})
	ctx, conn := protectedEndpoint(t, access, configs, routes)
	hostClient, gatewayClient := configsync.NewConfigSyncClient(conn), gateway.NewGatewayRoutesClient(conn)
	_, err := hostClient.RequestFullConfig(ctx, &configsync.FullConfigRequest{HostId: "execution-host"})
	assertCode(t, err, codes.Unauthenticated)
	_, err = hostClient.RequestFullConfig(bearer(ctx, gatewayToken), &configsync.FullConfigRequest{HostId: "execution-host"})
	assertCode(t, err, codes.PermissionDenied)
	_, err = gatewayClient.RequestFullRoutes(bearer(ctx, hostToken), &gateway.FullRoutesRequest{GatewayId: "gateway"})
	assertCode(t, err, codes.PermissionDenied)
	_, err = hostClient.RequestFullConfig(bearer(ctx, hostToken), &configsync.FullConfigRequest{})
	assertCode(t, err, codes.Unavailable)
	_, err = gatewayClient.RequestFullRoutes(bearer(ctx, gatewayToken), &gateway.FullRoutesRequest{})
	assertCode(t, err, codes.Unavailable)

	app := wasmplatformv1alpha1.Application{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "current", Generation: 2}}
	publication.Reconciled(key, "old", 2)
	publication.Initialize(ctx, []wasmplatformv1alpha1.Application{app})
	publication.Reconciled(key, app.UID, 1)
	_, err = hostClient.RequestFullConfig(bearer(ctx, hostToken), &configsync.FullConfigRequest{})
	assertCode(t, err, codes.Unavailable)
	publication.Reconciled(key, app.UID, app.Generation)
	response, err := hostClient.RequestFullConfig(bearer(ctx, hostToken), &configsync.FullConfigRequest{HostId: "gateway"})
	if err != nil || !response.Success || len(response.Config.Applications) != 1 {
		t.Fatalf("initialized host snapshot failed: %v", err)
	}
	if _, err := gatewayClient.RequestFullRoutes(bearer(ctx, gatewayToken), &gateway.FullRoutesRequest{}); err != nil {
		t.Fatal(err)
	}
	publication.Revoke()
	_, err = hostClient.RequestFullConfig(bearer(ctx, hostToken), &configsync.FullConfigRequest{})
	assertCode(t, err, codes.Unavailable)
}

func TestStreamsCloseOnAuthorityLoss(t *testing.T) {
	for _, service := range []string{"host", "gateway"} {
		for _, phase := range []string{"identification", "acknowledgement", "idle"} {
			t.Run(service+"/"+phase, func(t *testing.T) {
				leaderCtx, stopLeader := context.WithCancel(t.Context())
				defer stopLeader()
				publication := authority.New()
				publication.Initialize(leaderCtx, nil)
				hostToken := projectedToken("host", time.Now().Add(time.Hour))
				gatewayToken := projectedToken("gateway", time.Now().Add(time.Hour))
				access := newTestAccess(t, publication, workloadReviewer(t, hostToken, gatewayToken, nil))
				configs, routes := configstore.New(), routestore.New()
				ctx, conn := protectedEndpoint(t, access, configs, routes)
				var receive func() error
				if service == "host" {
					stream, err := configsync.NewConfigSyncClient(conn).PushIncrementalUpdate(bearer(ctx, hostToken))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := stream.Header(); err != nil {
						t.Fatal(err)
					}
					receive = func() error { _, err := stream.Recv(); return err }
					if phase != "identification" {
						if err := stream.Send(&configsync.IncrementalUpdateAck{HostId: "host"}); err != nil {
							t.Fatal(err)
						}
						waitForStreamUpdate(t, ctx, func() { configs.BroadcastUpdate(&configsync.IncrementalConfig{Version: "1"}) }, receive)
						if phase == "idle" {
							if err := stream.Send(&configsync.IncrementalUpdateAck{}); err != nil {
								t.Fatal(err)
							}
						}
					}
				} else {
					stream, err := gateway.NewGatewayRoutesClient(conn).PushRouteUpdate(bearer(ctx, gatewayToken))
					if err != nil {
						t.Fatal(err)
					}
					if _, err := stream.Header(); err != nil {
						t.Fatal(err)
					}
					receive = func() error { _, err := stream.Recv(); return err }
					if phase != "identification" {
						if err := stream.Send(&gateway.RouteUpdateAck{GatewayId: "gateway"}); err != nil {
							t.Fatal(err)
						}
						waitForStreamUpdate(t, ctx, func() { routes.BroadcastUpdate(&routestore.RouteUpdateBatch{Version: "1"}) }, receive)
						if phase == "idle" {
							if err := stream.Send(&gateway.RouteUpdateAck{}); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				stopLeader()
				assertCode(t, receive(), codes.Unavailable)
			})
		}
	}
}

func TestStreamsRevalidateRevokedToken(t *testing.T) {
	publication := authority.New()
	publication.Initialize(t.Context(), nil)
	var revoked atomic.Bool
	hostToken := projectedToken("host", time.Now().Add(time.Hour))
	gatewayToken := projectedToken("gateway", time.Now().Add(time.Hour))
	access := newTestAccess(t, publication, workloadReviewer(t, hostToken, gatewayToken, &revoked))
	access.revalidateAfter = time.Millisecond
	ctx, conn := protectedEndpoint(t, access, configstore.New(), routestore.New())
	stream, err := configsync.NewConfigSyncClient(conn).PushIncrementalUpdate(bearer(ctx, hostToken))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Header(); err != nil {
		t.Fatal(err)
	}
	revoked.Store(true)
	_, err = stream.Recv()
	assertCode(t, err, codes.Unauthenticated)
}

func TestStreamStopsAtTokenExpiry(t *testing.T) {
	publication := authority.New()
	publication.Initialize(t.Context(), nil)
	hostToken := projectedToken("host", time.Now().Add(2*time.Second))
	gatewayToken := projectedToken("gateway", time.Now().Add(time.Hour))
	access := newTestAccess(t, publication, workloadReviewer(t, hostToken, gatewayToken, nil))
	ctx, conn := protectedEndpoint(t, access, configstore.New(), routestore.New())
	stream, err := configsync.NewConfigSyncClient(conn).PushIncrementalUpdate(bearer(ctx, hostToken))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Header(); err != nil {
		t.Fatal(err)
	}
	_, err = stream.Recv()
	assertCode(t, err, codes.Unauthenticated)
}
