package cloudmcp

import (
	"context"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Permission reductions and org disable cancel idle streams as well as active
// requests. Checks run every five seconds with a five-second timeout; failures
// cancel the stream instead of extending the last known grant.
func (s *Server) watchAuthorization(parent context.Context, device, method string, expected Access) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check, stop := context.WithTimeout(ctx, 5*time.Second)
				current, err := s.authorize(check, device, method)
				stop()
				if err != nil || !current.sameIdentity(expected) {
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel
}

func (s *Server) serveTunnel(w http.ResponseWriter, r *http.Request, device string, access Access) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), minTime(access.ExpiresAt, time.Now().Add(time.Hour)))
	defer cancel()
	ctx, stop := s.watchAuthorization(ctx, device, "cli.connect", access)
	defer stop()
	ctx, finishTunnel, auditErr := s.audited(ctx, "mcp.cli_connection", access, attribute.String("wendy.device_id", device))
	if auditErr != nil {
		http.Error(w, "audit unavailable", http.StatusServiceUnavailable)
		return
	}
	outcome := "error"
	defer func() { finishTunnel(outcome) }()
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	outcome = "closed"
	ws.SetReadLimit(4 << 20)
	conn := &leaseConn{Conn: websocket.NetConn(ctx, ws, websocket.MessageBinary), cancel: cancel}
	listener := &singleListener{ctx: ctx, conn: conn}
	proxy := grpc.NewServer(grpc.ForceServerCodec(rawCodec{}), grpc.UnknownServiceHandler(s.proxyDevice(ctx, device)), grpc.MaxRecvMsgSize(4<<20), grpc.MaxSendMsgSize(4<<20), grpc.MaxConcurrentStreams(16))
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			proxy.Stop()
		case <-done:
		}
	}()
	_ = proxy.Serve(listener)
	close(done)
	proxy.Stop()
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

type singleListener struct {
	ctx   context.Context
	conn  net.Conn
	mu    sync.Mutex
	taken bool
}

type leaseConn struct {
	net.Conn
	cancel context.CancelFunc
}

func (c *leaseConn) Close() error { c.cancel(); return c.Conn.Close() }

func (l *singleListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	first := !l.taken
	l.taken = true
	l.mu.Unlock()
	if first {
		return l.conn, nil
	}
	<-l.ctx.Done()
	return nil, net.ErrClosed
}
func (l *singleListener) Close() error   { return l.conn.Close() }
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }

type rawMessage []byte
type rawCodec struct{}

func (rawCodec) Name() string { return "proto" }
func (rawCodec) Marshal(value any) ([]byte, error) {
	m, ok := value.(*rawMessage)
	if !ok {
		return nil, fmt.Errorf("invalid proxy message")
	}
	return *m, nil
}
func (rawCodec) Unmarshal(data []byte, value any) error {
	m, ok := value.(*rawMessage)
	if !ok {
		return fmt.Errorf("invalid proxy message")
	}
	*m = append((*m)[:0], data...)
	return nil
}

// The public tunnel terminates gRPC here. It is not an unrestricted byte relay:
// each method is identified and authorized before opening an operator connection.
func (s *Server) proxyDevice(callerContext context.Context, device string) grpc.StreamHandler {
	return func(_ any, downstream grpc.ServerStream) (rpcError error) {
		method, ok := grpc.MethodFromServerStream(downstream)
		if !ok {
			return status.Error(codes.InvalidArgument, "missing method")
		}
		if _, err := agentMethod(method); err != nil {
			return status.Error(codes.PermissionDenied, "not an agent RPC")
		}
		ctx, cancel := context.WithCancel(downstream.Context())
		defer cancel()
		identity, ok := callerContext.Value(callerKey{}).(caller)
		if !ok {
			return status.Error(codes.Unauthenticated, "missing caller")
		}
		ctx = context.WithValue(ctx, callerKey{}, identity)
		ctx = context.WithValue(ctx, correlationKey{}, callerContext.Value(correlationKey{}))
		ctx = trace.ContextWithSpanContext(ctx, trace.SpanContextFromContext(callerContext))
		access, err := s.authorize(ctx, device, method)
		if err != nil {
			return status.Error(codes.PermissionDenied, "device operation denied")
		}
		ctx, finishRPC, auditErr := s.audited(ctx, "mcp.cli_rpc", access, attribute.String("rpc.method", method), attribute.String("wendy.device_id", device))
		if auditErr != nil {
			return status.Error(codes.Unavailable, "audit unavailable")
		}
		rpcError = status.Error(codes.Unknown, "execution interrupted")
		defer func() {
			if rpcError == nil {
				finishRPC("ok")
			} else {
				finishRPC("error")
			}
		}()
		ctx, stopExpiry := context.WithDeadline(ctx, access.ExpiresAt)
		defer stopExpiry()
		ctx, stop := s.watchAuthorization(ctx, device, method, access)
		defer stop()
		conn, closeConnection, err := s.connect(ctx, access, device)
		if err != nil {
			return status.Error(codes.Unavailable, "device connection unavailable")
		}
		defer closeConnection()
		// Only application routing metadata crosses into the operator connection.
		// User tokens, asserted identities, and signatures never do.
		incoming, _ := metadata.FromIncomingContext(downstream.Context())
		outgoing := metadata.MD{}
		for _, name := range []string{"app-name", "x-wendy-ros2-scope", "x-wendy-ros2-lidar", "x-wendy-ros2-lidar-options"} {
			if values := incoming.Get(name); len(values) > 0 {
				outgoing[name] = append([]string(nil), values...)
			}
		}
		ctx = deviceTraceContext(metadata.NewOutgoingContext(ctx, outgoing))
		upstream, err := conn.Conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, method, grpc.ForceCodec(rawCodec{}))
		if err != nil {
			return status.Error(codes.Unavailable, "device stream unavailable")
		}
		go func() {
			for {
				var message rawMessage
				err := downstream.RecvMsg(&message)
				if errors.Is(err, io.EOF) {
					_ = upstream.CloseSend()
					return
				}
				if err != nil {
					cancel()
					return
				}
				if current, err := s.authorize(ctx, device, method); err != nil || !current.sameIdentity(access) {
					cancel()
					return
				}
				if err := upstream.SendMsg(&message); err != nil {
					if !errors.Is(err, io.EOF) {
						cancel()
					}
					return
				}
			}
		}()
		if header, err := upstream.Header(); err == nil {
			_ = downstream.SendHeader(header)
		}
		for {
			var message rawMessage
			err := upstream.RecvMsg(&message)
			if err != nil {
				downstream.SetTrailer(upstream.Trailer())
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			if current, err := s.authorize(ctx, device, method); err != nil || !current.sameIdentity(access) {
				return status.Error(codes.PermissionDenied, "device operation no longer authorized")
			}
			if err := downstream.SendMsg(&message); err != nil {
				return err
			}
		}
	}
}
