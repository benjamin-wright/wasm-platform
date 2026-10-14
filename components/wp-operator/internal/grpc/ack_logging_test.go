package grpcserver

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	gateway "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/gateway"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/routestore"
	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	ctrl "sigs.k8s.io/controller-runtime"
)

func TestInitialAcksDoNotLogFreeFormMessages(t *testing.T) {
	secretMessage := "postgres://app:sql-password@db/app redis://app:redis-password@redis Bearer private-token"

	t.Run("execution host", func(t *testing.T) {
		store := configstore.New()
		ctx, conn, logs := newLoggedGRPCServer(t, func(server *grpc.Server) {
			Register(server, store)
		})
		stream, err := configsync.NewConfigSyncClient(conn).PushIncrementalUpdate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&configsync.IncrementalUpdateAck{
			HostId: "host-1", VersionApplied: "1", Success: true, Message: secretMessage,
		}); err != nil {
			t.Fatal(err)
		}

		waitForStreamUpdate(t, ctx, func() {
			store.BroadcastUpdate(&configsync.IncrementalConfig{Version: "2"})
		}, func() error {
			update, err := stream.Recv()
			if err == nil && update.IncrementalConfig.Version != "2" {
				return fmt.Errorf("received config version %q", update.IncrementalConfig.Version)
			}
			return err
		})
		defer store.DeregisterHost("host-1")
		assertSafeAckLogs(t, logs, "received initial host ack", "host_id", "host-1")
	})

	t.Run("gateway", func(t *testing.T) {
		store := routestore.New()
		ctx, conn, logs := newLoggedGRPCServer(t, func(server *grpc.Server) {
			RegisterGateway(server, store)
		})
		stream, err := gateway.NewGatewayRoutesClient(conn).PushRouteUpdate(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := stream.Send(&gateway.RouteUpdateAck{
			GatewayId: "gateway-1", VersionApplied: "1", Success: true, Message: secretMessage,
		}); err != nil {
			t.Fatal(err)
		}

		waitForStreamUpdate(t, ctx, func() {
			store.BroadcastUpdate(&routestore.RouteUpdateBatch{Version: "2"})
		}, func() error {
			update, err := stream.Recv()
			if err == nil && update.Update.Version != "2" {
				return fmt.Errorf("received route version %q", update.Update.Version)
			}
			return err
		})
		defer store.DeregisterGateway("gateway-1")
		assertSafeAckLogs(t, logs, "received initial gateway ack", "gateway_id", "gateway-1")
	})
}

func newLoggedGRPCServer(t *testing.T, register func(*grpc.Server), options ...grpc.ServerOption) (context.Context, *grpc.ClientConn, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	previousLogger := ctrl.Log
	ctrl.Log = zapr.NewLogger(zap.New(core))
	t.Cleanup(func() { ctrl.Log = previousLogger })

	listener := bufconn.Listen(1024 * 1024)
	server := grpc.NewServer(options...)
	register(server)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})

	conn, err := grpc.NewClient("passthrough:///ack-log-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx, conn, logs
}

func waitForStreamUpdate(t *testing.T, ctx context.Context, publish func(), receive func() error) {
	t.Helper()
	received := make(chan error, 1)
	go func() { received <- receive() }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case err := <-received:
			if err != nil {
				t.Fatal(err)
			}
			return
		case <-ticker.C:
			publish()
		case <-ctx.Done():
			t.Fatal("stream did not deliver an update before its deadline")
		}
	}
}

func assertSafeAckLogs(t *testing.T, logs *observer.ObservedLogs, message, identityField, identity string) {
	t.Helper()
	acks := logs.FilterMessage(message).All()
	if len(acks) != 1 {
		t.Fatalf("expected one initial ACK log, got %d", len(acks))
	}
	fields := acks[0].ContextMap()
	if len(fields) != 2 || fields[identityField] != identity || fields["success"] != true {
		t.Fatalf("unexpected ACK fields: %v", fields)
	}
	for _, entry := range logs.All() {
		output := fmt.Sprint(entry.Message, entry.ContextMap())
		for _, secret := range []string{"sql-password", "redis-password", "private-token"} {
			if strings.Contains(output, secret) {
				t.Fatalf("secret %q appeared in stream logs", secret)
			}
		}
	}
}
