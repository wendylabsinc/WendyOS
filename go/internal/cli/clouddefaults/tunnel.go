package clouddefaults

import (
	"context"
	"errors"
	"net"

	"google.golang.org/grpc"
)

// TunnelDialer opens a fresh broker stream on every gRPC connection attempt.
// Reusing a net.Conn here strands all subsequent RPCs on a closed pipe after
// the first failed TLS handshake or disconnected transport.
func TunnelDialer(open func(context.Context) (net.Conn, error)) grpc.DialOption {
	return grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return dialTunnel(ctx, open)
	})
}

func dialTunnel(ctx context.Context, open func(context.Context) (net.Conn, error)) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// gRPC cancels its dial context after establishing the transport. A broker
	// stream must survive that cancellation and end when the connection closes.
	// Keep cancellation linked during open so an abandoned dial cannot leak.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	conn, err := open(streamCtx)
	stop()
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		cancel()
		if conn != nil {
			_ = conn.Close()
		}
		return nil, &tunnelDialError{reason: err}
	}
	return &tunnelConn{Conn: conn, cancel: cancel}, nil
}

// A tunnel authorization RPC can return a status that gRPC forbids its picker
// from returning. Keep the reason, but hide its status from errors.As so a
// failed dial becomes Unavailable rather than an "illegal status" Internal.
// Like TunnelClosedError, this deliberately has no Unwrap method.
type tunnelDialError struct {
	reason error
}

func (e *tunnelDialError) Error() string        { return e.reason.Error() }
func (e *tunnelDialError) Is(target error) bool { return errors.Is(e.reason, target) }

type tunnelConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *tunnelConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}
