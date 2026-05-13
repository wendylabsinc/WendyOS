package interceptor

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func isCanceled(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	s, ok := status.FromError(err)
	return ok && s.Code() == codes.Canceled
}

// UnaryErrorInterceptor returns a gRPC unary server interceptor that recovers
// from panics and logs handler errors.
func UnaryErrorInterceptor(logger *zap.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp interface{}, err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("Panic recovered in gRPC handler",
					zap.String("method", info.FullMethod),
					zap.Any("panic", r),
					zap.String("stack", string(debug.Stack())),
				)
				err = status.Errorf(codes.Internal, "internal error")
			}
		}()

		resp, err = handler(ctx, req)
		if err != nil {
			logger.Error("gRPC handler error",
				zap.String("method", info.FullMethod),
				zap.Error(err),
			)
		}
		return
	}
}

// wrappedStream wraps a grpc.ServerStream to intercept RecvMsg/SendMsg for panic recovery.
type wrappedStream struct {
	grpc.ServerStream
	logger *zap.Logger
	method string
}

func (w *wrappedStream) RecvMsg(m interface{}) error {
	return w.ServerStream.RecvMsg(m)
}

func (w *wrappedStream) SendMsg(m interface{}) error {
	return w.ServerStream.SendMsg(m)
}

// StreamErrorInterceptor returns a gRPC stream server interceptor that recovers
// from panics and logs handler errors.
func StreamErrorInterceptor(logger *zap.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) (err error) {
		defer func() {
			if r := recover(); r != nil {
				logger.Error("Panic recovered in gRPC stream handler",
					zap.String("method", info.FullMethod),
					zap.String("panic", fmt.Sprintf("%v", r)),
					zap.String("stack", string(debug.Stack())),
				)
				err = status.Errorf(codes.Internal, "internal error")
			}
		}()

		wrapped := &wrappedStream{
			ServerStream: ss,
			logger:       logger,
			method:       info.FullMethod,
		}

		err = handler(srv, wrapped)
		if err != nil && !isCanceled(err) {
			logger.Error("gRPC stream handler error",
				zap.String("method", info.FullMethod),
				zap.Error(err),
			)
		}
		return
	}
}
