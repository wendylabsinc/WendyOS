package clouddefaults

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TunnelClosedError reports why the broker ended a tunnel stream. Reason holds
// the broker's verdict (usually a gRPC status) and errors.Is sees through to it,
// but the type deliberately does not Unwrap: gRPC's picker discovers a wrapped
// status through errors.As and would then end RPCs with the broker's code and
// hand callers the raw connection-error text, instead of treating a failed
// handshake as the transport failure (Unavailable) it is. Read the verdict with
// errors.As on this type, or ExplainTunnelClose on the message.
type TunnelClosedError struct {
	Reason error
}

// tunnelClosedMarker prefixes the broker's verdict in TunnelClosedError's text;
// ExplainTunnelClose finds it again inside whatever envelope wrapped it.
const tunnelClosedMarker = "cloud tunnel closed by broker: "

func (e *TunnelClosedError) Error() string {
	return tunnelClosedMarker + e.Reason.Error()
}

// ExplainTunnelClose extracts the broker's verdict from an error message that
// carries a TunnelClosedError somewhere inside its transport envelope, such as
// gRPC's `connection error: desc = "transport: authentication handshake failed:
// cloud tunnel closed by broker: rpc error: code = X desc = Y"`. A gRPC status
// verdict comes back as "Y (X)"; any other reason comes back as written.
func ExplainTunnelClose(msg string) (string, bool) {
	idx := strings.Index(msg, tunnelClosedMarker)
	if idx < 0 {
		return "", false
	}
	verdict := strings.TrimRight(msg[idx+len(tunnelClosedMarker):], `"`)
	const codePrefix, descSep = "rpc error: code = ", " desc = "
	if rest, ok := strings.CutPrefix(verdict, codePrefix); ok {
		if code, desc, found := strings.Cut(rest, descSep); found {
			return desc + " (" + code + ")", true
		}
	}
	return verdict, true
}

// Is lets errors.Is match the broker's verdict without exposing it to
// errors.As (see the type comment).
func (e *TunnelClosedError) Is(target error) bool { return errors.Is(e.Reason, target) }

// BrokerTunnelConn is the local end of a broker tunnel pipe. The pump that
// reads the broker stream records the stream's terminal error with Fail; from
// then on the EOF or closed-pipe error the pipe would hand back is replaced by
// that verdict. Without it a tunnel the broker refused (unauthorized caller,
// asset offline, ...) reaches the user only as "authentication handshake
// failed: EOF" from the TLS layer riding on the pipe.
type BrokerTunnelConn struct {
	net.Conn

	mu     sync.Mutex
	reason error
}

// NewBrokerTunnelConn wraps the local end of a tunnel pipe.
func NewBrokerTunnelConn(c net.Conn) *BrokerTunnelConn {
	return &BrokerTunnelConn{Conn: c}
}

// Fail records the broker's verdict for this tunnel. A clean end (nil, io.EOF)
// or a cancellation is not a verdict and is ignored; the first verdict wins.
func (c *BrokerTunnelConn) Fail(err error) {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) || status.Code(err) == codes.Canceled {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reason == nil {
		c.reason = err
	}
}

func (c *BrokerTunnelConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	return n, c.explain(err)
}

func (c *BrokerTunnelConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	return n, c.explain(err)
}

// explain swaps the pipe's own end-of-stream errors for the recorded verdict.
// Any other error (a deadline, for instance) is the caller's own and passes
// through untouched.
func (c *BrokerTunnelConn) explain(err error) error {
	if err == nil || !(errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe)) {
		return err
	}
	c.mu.Lock()
	reason := c.reason
	c.mu.Unlock()
	if reason == nil {
		return err
	}
	return &TunnelClosedError{Reason: reason}
}
