package grpcserver

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	gateway "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/gateway"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/routestore"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// GatewayServer implements the gRPC GatewayRoutes service.
type GatewayServer struct {
	gateway.UnimplementedGatewayRoutesServer
	store *routestore.Store
}

// NewGateway returns a new GatewayServer backed by the given routestore.Store.
func NewGateway(store *routestore.Store) *GatewayServer {
	return &GatewayServer{store: store}
}

// RegisterGateway registers the GatewayServer with the provided gRPC server instance.
func RegisterGateway(grpcSrv *grpc.Server, store *routestore.Store) {
	gateway.RegisterGatewayRoutesServer(grpcSrv, NewGateway(store))
}

// RequestFullRoutes returns a snapshot of all RouteConfig values currently held in the store.
func (s *GatewayServer) RequestFullRoutes(_ context.Context, req *gateway.FullRoutesRequest) (*gateway.FullRoutesResponse, error) {
	log := ctrl.Log.WithName("grpc.RequestFullRoutes")
	log.Info("RequestFullRoutes called", "gateway_id", req.GatewayId)
	routes := s.store.Snapshot()
	protoRoutes := make([]*gateway.RouteConfig, len(routes))
	for i, r := range routes {
		protoRoutes[i] = routeToProto(r)
	}
	version := fmt.Sprintf("%d", s.store.Version())

	return &gateway.FullRoutesResponse{
		Success:   true,
		Routes:    protoRoutes,
		Version:   version,
		Timestamp: time.Now().UnixMilli(),
	}, nil
}

// PushRouteUpdate handles the bidirectional streaming RPC.
// The gateway sends RouteUpdateAck messages (the first identifies the gateway);
// the operator pushes RouteUpdateRequest messages back.
// On stream close or error the gateway is deregistered so it can reconnect via RequestFullRoutes.
func (s *GatewayServer) PushRouteUpdate(stream grpc.BidiStreamingServer[gateway.RouteUpdateAck, gateway.RouteUpdateRequest]) error {
	log := ctrl.Log.WithName("grpc.PushRouteUpdate")
	log.Info("PushRouteUpdate stream handler entered")

	// Send HTTP/2 response headers immediately so tonic's .await on the RPC
	// call can complete. Without this, tonic waits for headers while Go waits
	// for Recv() — a deadlock.
	if err := stream.SendHeader(metadata.MD{}); err != nil {
		log.Error(err, "failed to send initial response headers")
		return err
	}

	// The first message from the gateway tells us its identity.
	log.Info("waiting for initial gateway ack")
	firstAck, err := stream.Recv()
	if err != nil {
		log.Error(err, "failed to receive initial gateway ack")
		return err
	}
	log.Info("received initial gateway ack", "ack", firstAck)
	gatewayID := firstAck.GatewayId
	if gatewayID == "" {
		gatewayID = "unknown"
	}
	log.Info("gateway connected", "gateway_id", gatewayID)

	ch := s.store.RegisterGateway(gatewayID)
	defer func() {
		s.store.DeregisterGateway(gatewayID)
		log.Info("gateway disconnected", "gateway_id", gatewayID)
	}()

	for batch := range ch {
		protoUpdates := make([]*gateway.RouteUpdate, len(batch.Updates))
		for i, u := range batch.Updates {
			protoUpdates[i] = &gateway.RouteUpdate{
				Route:  routeToProto(u.Config),
				Delete: u.Delete,
			}
		}
		req := &gateway.RouteUpdateRequest{
			TargetGatewayId: gatewayID,
			Update: &gateway.RouteUpdateConfig{
				Version:   batch.Version,
				Updates:   protoUpdates,
				Timestamp: batch.Timestamp,
			},
		}
		if sendErr := stream.Send(req); sendErr != nil {
			log.Error(sendErr, "failed to send route update", "gateway_id", gatewayID)
			return sendErr
		}
		// Wait for the gateway's acknowledgement before delivering the next update.
		if _, recvErr := stream.Recv(); recvErr != nil {
			log.Error(recvErr, "failed to receive route ack", "gateway_id", gatewayID)
			return recvErr
		}
	}

	// Channel closed by slow-gateway eviction in BroadcastUpdate; gateway should reconnect.
	return nil
}

func routeToProto(r *routestore.RouteConfig) *gateway.RouteConfig {
	if r == nil {
		return nil
	}
	return &gateway.RouteConfig{
		Path:        r.Path,
		Methods:     r.Methods,
		NatsSubject: r.NatsSubject,
	}
}
