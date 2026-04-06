package grpcserver

import (
	"context"
	"fmt"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/configstore"
	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// Server implements the gRPC ConfigSync service.
type Server struct {
	configsync.UnimplementedConfigSyncServer
	store *configstore.Store
}

// New returns a new Server backed by the given Store.
func New(store *configstore.Store) *Server {
	return &Server{store: store}
}

// Register registers the Server with the provided gRPC server instance.
func Register(grpcSrv *grpc.Server, store *configstore.Store) {
	configsync.RegisterConfigSyncServer(grpcSrv, New(store))
}

// RequestFullConfig returns a snapshot of all ApplicationConfig values currently held in the store.
func (s *Server) RequestFullConfig(_ context.Context, req *configsync.FullConfigRequest) (*configsync.FullConfigResponse, error) {
	log := ctrl.Log.WithName("grpc.RequestFullConfig")
	log.Info("RequestFullConfig called", "host_id", req.HostId)
	apps := s.store.Snapshot()
	version := fmt.Sprintf("%d", s.store.Version())

	return &configsync.FullConfigResponse{
		Success: true,
		Config: &configsync.FullConfig{
			Version:      version,
			Applications: apps,
			Timestamp:    time.Now().UnixMilli(),
		},
	}, nil
}

// PushIncrementalUpdate handles the bidirectional streaming RPC.
// The host sends IncrementalUpdateAck messages (the first identifies the host);
// the operator pushes IncrementalUpdateRequest messages back.
// On stream close or error the host is deregistered so it can reconnect via RequestFullConfig.
func (s *Server) PushIncrementalUpdate(stream grpc.BidiStreamingServer[configsync.IncrementalUpdateAck, configsync.IncrementalUpdateRequest]) error {
	log := ctrl.Log.WithName("grpc.PushIncrementalUpdate")
	log.Info("PushIncrementalUpdate stream handler entered")

	// Send HTTP/2 response headers immediately so tonic's .await on the RPC
	// call can complete. Without this, tonic waits for headers while Go waits
	// for Recv() — a deadlock.
	if err := stream.SendHeader(metadata.MD{}); err != nil {
		log.Error(err, "failed to send initial response headers")
		return err
	}

	// The first message from the host tells us its identity.
	log.Info("waiting for initial host ack")
	firstAck, err := stream.Recv()
	if err != nil {
		log.Error(err, "failed to receive initial host ack")
		return err
	}
	log.Info("received initial host ack", "ack", firstAck)
	hostID := firstAck.HostId
	if hostID == "" {
		hostID = "unknown"
	}
	log.Info("host connected", "host_id", hostID)

	ch := s.store.RegisterHost(hostID)
	defer func() {
		s.store.DeregisterHost(hostID)
		log.Info("host disconnected", "host_id", hostID)
	}()

	for update := range ch {
		req := &configsync.IncrementalUpdateRequest{
			TargetHostId:      hostID,
			IncrementalConfig: update,
		}
		if sendErr := stream.Send(req); sendErr != nil {
			log.Error(sendErr, "failed to send incremental update", "host_id", hostID)
			return sendErr
		}
		// Wait for the host's acknowledgement before delivering the next update.
		if _, recvErr := stream.Recv(); recvErr != nil {
			log.Error(recvErr, "failed to receive ack", "host_id", hostID)
			return recvErr
		}
	}

	// Channel closed by slow-host eviction in BroadcastUpdate; host should reconnect.
	return nil
}
