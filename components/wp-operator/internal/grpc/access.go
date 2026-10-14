package grpcserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/authority"
	configsync "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/configsync"
	gateway "github.com/benjamin-wright/wasm-platform/wp-operator/internal/grpc/gateway"
	"github.com/benjamin-wright/wasm-platform/wp-operator/internal/identity"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	snapshotTimeout = 10 * time.Second
	maxSessionTime  = 5 * time.Minute
	reviewInterval  = 30 * time.Second
)

// Access authenticates recipient roles and binds every RPC to current publication authority.
type Access struct {
	reviewer        identity.Reviewer
	authority       *authority.Authority
	host            string
	gateway         string
	revalidateAfter time.Duration
}

func NewAccess(reviewer identity.Reviewer, publication *authority.Authority, namespace, hostServiceAccount, gatewayServiceAccount string) (*Access, error) {
	if reviewer == nil || publication == nil {
		return nil, errors.New("workload reviewer and publication authority are required")
	}
	if namespace == "" || len(validation.IsDNS1123Label(namespace)) != 0 {
		return nil, errors.New("POD_NAMESPACE must be a Kubernetes namespace")
	}
	for _, account := range []string{hostServiceAccount, gatewayServiceAccount} {
		if account == "" || len(validation.IsDNS1123Subdomain(account)) != 0 {
			return nil, errors.New("recipient ServiceAccount names must be configured")
		}
	}
	if hostServiceAccount == gatewayServiceAccount {
		return nil, errors.New("execution host and gateway require distinct ServiceAccounts")
	}
	return &Access{
		reviewer: reviewer, authority: publication,
		revalidateAfter: reviewInterval,
		host:            fmt.Sprintf("system:serviceaccount:%s:%s", namespace, hostServiceAccount),
		gateway:         fmt.Sprintf("system:serviceaccount:%s:%s", namespace, gatewayServiceAccount),
	}, nil
}

func (a *Access) authenticate(ctx context.Context, method string) (identity.Recipient, string, error) {
	var allowedUsername string
	switch method {
	case configsync.ConfigSync_RequestFullConfig_FullMethodName, configsync.ConfigSync_PushIncrementalUpdate_FullMethodName:
		allowedUsername = a.host
	case gateway.GatewayRoutes_RequestFullRoutes_FullMethodName, gateway.GatewayRoutes_PushRouteUpdate_FullMethodName:
		allowedUsername = a.gateway
	default:
		return identity.Recipient{}, "", status.Error(codes.PermissionDenied, "recipient method is not authorized")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get("authorization")
	if len(values) != 1 {
		return identity.Recipient{}, "", status.Error(codes.Unauthenticated, "one workload bearer token is required")
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return identity.Recipient{}, "", status.Error(codes.Unauthenticated, "workload bearer token is required")
	}
	recipient, err := a.reviewer.Review(ctx, parts[1])
	if err != nil {
		return identity.Recipient{}, "", reviewError(err)
	}
	if recipient.Username != allowedUsername {
		return identity.Recipient{}, "", status.Error(codes.PermissionDenied, "workload role cannot receive this configuration")
	}
	if !recipient.ExpiresAt.After(time.Now()) {
		return identity.Recipient{}, "", status.Error(codes.Unauthenticated, "workload token has expired")
	}
	return recipient, parts[1], nil
}

func reviewError(err error) error {
	if errors.Is(err, identity.ErrUnavailable) {
		return status.Error(codes.Unavailable, "workload authentication is unavailable")
	}
	return status.Error(codes.Unauthenticated, "workload token is not valid")
}

func (a *Access) session(ctx context.Context, recipient identity.Recipient, limit time.Duration) (context.Context, func(), error) {
	authorityCtx, err := a.authority.Context()
	if err != nil {
		return nil, nil, status.Error(codes.Unavailable, "configuration authority is not initialized")
	}
	deadline := time.Now().Add(limit)
	expirationError := status.Error(codes.Unavailable, "configuration session requires renewal")
	if recipient.ExpiresAt.Before(deadline) {
		deadline = recipient.ExpiresAt
		expirationError = status.Error(codes.Unauthenticated, "workload token has expired")
	}
	deadlineCtx, deadlineCancel := context.WithDeadlineCause(ctx, deadline, expirationError)
	sessionCtx, sessionCancel := context.WithCancelCause(deadlineCtx)
	stop := context.AfterFunc(authorityCtx, func() {
		sessionCancel(status.Error(codes.Unavailable, "configuration publication authority was lost"))
	})
	return sessionCtx, func() { stop(); sessionCancel(nil); deadlineCancel() }, nil
}

func (a *Access) UnaryInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	recipient, _, err := a.authenticate(ctx, info.FullMethod)
	if err != nil {
		return nil, err
	}
	sessionCtx, closeSession, err := a.session(ctx, recipient, snapshotTimeout)
	if err != nil {
		return nil, err
	}
	defer closeSession()
	response, err := handler(sessionCtx, req)
	if sessionCtx.Err() != nil {
		return nil, context.Cause(sessionCtx)
	}
	return response, err
}

func (a *Access) StreamInterceptor(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	recipient, token, err := a.authenticate(stream.Context(), info.FullMethod)
	if err != nil {
		return err
	}
	sessionCtx, closeSession, err := a.session(stream.Context(), recipient, maxSessionTime)
	if err != nil {
		return err
	}
	defer closeSession()
	reviewCtx, cancelReview := context.WithCancelCause(sessionCtx)
	defer cancelReview(nil)
	go a.revalidate(reviewCtx, cancelReview, recipient, token)
	err = handler(srv, &protectedStream{ServerStream: stream, ctx: reviewCtx})
	if reviewCtx.Err() != nil {
		return context.Cause(reviewCtx)
	}
	return err
}

func (a *Access) revalidate(ctx context.Context, cancel context.CancelCauseFunc, initial identity.Recipient, token string) {
	ticker := time.NewTicker(a.revalidateAfter)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			recipient, err := a.reviewer.Review(ctx, token)
			if err != nil {
				cancel(reviewError(err))
				return
			}
			if recipient.Username != initial.Username || !recipient.ExpiresAt.After(time.Now()) {
				cancel(status.Error(codes.Unauthenticated, "workload token is not valid"))
				return
			}
		}
	}
}

type protectedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *protectedStream) Context() context.Context { return s.ctx }

func (s *protectedStream) SendMsg(message any) error {
	return s.interrupt(func() error { return s.ServerStream.SendMsg(message) })
}

func (s *protectedStream) RecvMsg(message any) error {
	return s.interrupt(func() error { return s.ServerStream.RecvMsg(message) })
}

// Returning from the handler cancels gRPC's transport context, releasing an in-progress
// receive or flow-controlled send that cannot observe the session's derived context.
func (s *protectedStream) interrupt(operation func() error) error {
	if s.ctx.Err() != nil {
		return context.Cause(s.ctx)
	}
	done := make(chan error, 1)
	go func() { done <- operation() }()
	select {
	case err := <-done:
		return err
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	}
}
