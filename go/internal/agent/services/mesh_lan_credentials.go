package services

import (
	"context"
	"net"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// gRPC wraps TLS and socket errors in Unavailable statuses. Preserve their
// actual causes so address retry never mistakes a certificate rejection for an
// unreachable interface, and does not discard a retryable socket failure.
type lanConnectionErrors struct {
	mu              sync.Mutex
	handshake, dial error
	stop            context.CancelFunc
}

func (e *lanConnectionErrors) recordDial(err error) {
	e.mu.Lock()
	if err != nil && e.dial == nil {
		e.dial = err
	}
	e.mu.Unlock()
	if err != nil && e.stop != nil {
		e.stop()
	}
}
func (e *lanConnectionErrors) recordHandshake(err error) {
	e.mu.Lock()
	if err != nil && e.handshake == nil {
		e.handshake = err
	}
	e.mu.Unlock()
	if err != nil && e.stop != nil {
		e.stop()
	}
}
func (e *lanConnectionErrors) cause(fallback error) error {
	if code := status.Code(fallback); code == codes.PermissionDenied || code == codes.Unauthenticated || code == codes.InvalidArgument {
		return fallback
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.handshake != nil {
		return e.handshake
	}
	if e.dial != nil {
		return e.dial
	}
	return fallback
}

type lanRecordingCredentials struct {
	credentials.TransportCredentials
	errors *lanConnectionErrors
}

func (c lanRecordingCredentials) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, info, err := c.TransportCredentials.ClientHandshake(ctx, authority, raw)
	c.errors.recordHandshake(err)
	return conn, info, err
}
func (c lanRecordingCredentials) Clone() credentials.TransportCredentials {
	return lanRecordingCredentials{c.TransportCredentials.Clone(), c.errors}
}
