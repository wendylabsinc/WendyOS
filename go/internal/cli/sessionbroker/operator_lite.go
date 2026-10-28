package sessionbroker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	operatorContextMethod  = "/wendy.sessionbroker.ContextV1/Status"
	operatorDescribeMethod = "/wendy.sessionbroker.ManagementV1/Describe"
	operatorLiteMethod     = "/wendy.sessionbroker.ManagementV1/LiteTCP"
	operatorTCPMethod      = "/wendy.sessionbroker.ManagementV1/TCP"
	operatorLiteChunk      = 16 * 1024
	operatorLiteStreams    = 8
)

// OperatorTargetInfo describes the authenticated adjacent router's current
// observation. Destination TLS still authenticates the exact selected device.
const operatorIPCVersion = 2

type operatorContextStatus struct {
	Version   int    `json:"version"`
	Principal string `json:"principal"`
	State     string `json:"state"`
	Source    string `json:"source,omitempty"`
}

type OperatorTargetInfo struct {
	Selector          string   `json:"selector"`
	Principal         string   `json:"principal"`
	Tenant            string   `json:"tenant"`
	Device            string   `json:"device"`
	Name              string   `json:"name,omitempty"`
	Platform          string   `json:"platform"`
	Transport         string   `json:"transport"`
	Address           string   `json:"address,omitempty"`
	Expires           int64    `json:"expiresAtUnixMs"`
	Revision          uint64   `json:"revision,string"`
	Metric            uint32   `json:"metric,omitempty"`
	NextHop           string   `json:"nextHopPrincipal,omitempty"`
	Caps              []string `json:"caps,omitempty"`
	OperatorPrincipal string   `json:"operatorPrincipal"`
}

func (r *operatorTargets) managementInfo(ctx context.Context) (OperatorTargetInfo, error) {
	values := metadata.ValueFromIncomingContext(ctx, operatorTargetHeader)
	if len(values) != 1 {
		return OperatorTargetInfo{}, status.Error(codes.InvalidArgument, "one mesh target is required")
	}
	target, matched, err := ParseMeshSelector(values[0])
	if err != nil || !matched {
		return OperatorTargetInfo{}, status.Error(codes.InvalidArgument, "invalid mesh target")
	}
	operator, err := operatorPrincipal(r.certificate)
	if err != nil || target.Tenant() != operator.Tenant() {
		return OperatorTargetInfo{}, status.Error(codes.InvalidArgument, "mesh target differs from the selected operator tenant")
	}
	// First attachment currently requires WendyOS. Its existing pinned management
	// stream identifies the adjacent target without requiring a routed hop.
	_, adjacent, err := r.adjacentPeer()
	if err != nil && r.localSnapshot == nil {
		return OperatorTargetInfo{}, err
	}
	if err == nil && target == adjacent {
		return OperatorTargetInfo{Selector: MeshSelector(target), Principal: target.Principal(), Tenant: target.Tenant().String(), Device: target.UUID().String(), Platform: "WendyOS", Transport: "mtls-v1", OperatorPrincipal: operator.Principal()}, nil
	}
	view, err := r.lookupTopology(ctx, target)
	if err != nil {
		return OperatorTargetInfo{}, fmt.Errorf("read attached router topology: %w", err)
	}
	reporter, err := r.reporter()
	if err != nil {
		return OperatorTargetInfo{}, err
	}
	info, err := operatorManagementTarget(view, reporter, target, time.Now())
	info.OperatorPrincipal = operator.Principal()
	return info, err
}

func (r *operatorTargets) localManagement(stream grpc.ServerStream, a *activity) (bool, error) {
	if handled, err := r.localApp(stream, a); handled {
		return true, err
	}
	if handled, err := r.localTopology(stream, a); handled {
		return true, err
	}
	if handled, err := r.localServices(stream, a); handled {
		return true, err
	}
	if handled, err := r.localDirectory(stream, a); handled {
		return true, err
	}
	method, _ := grpc.MethodFromServerStream(stream)
	if method != operatorContextMethod && method != operatorDescribeMethod && method != operatorLiteMethod && method != operatorTCPMethod {
		return false, nil
	}
	a.activeRPCs.Add(1)
	a.touch()
	defer func() { a.activeRPCs.Add(-1); a.touch() }()
	var request rawMessage
	if err := stream.RecvMsg(&request); err != nil {
		return true, err
	}
	if method != operatorTCPMethod && len(request) != 0 {
		return true, status.Error(codes.InvalidArgument, "management open has no payload")
	}
	ctx := stream.Context()
	if method == operatorContextMethod {
		operator, err := operatorPrincipal(r.certificate)
		if err != nil {
			return true, err
		}
		context := operatorContextStatus{Version: operatorIPCVersion, Principal: operator.Principal(), State: "reconnecting"}
		if _, peer, err := r.adjacentPeer(); err == nil {
			context.State, context.Source = "ready", peer.Principal()
		}
		data, err := json.Marshal(context)
		if err != nil {
			return true, err
		}
		reply := rawMessage(data)
		return true, stream.SendMsg(&reply)
	}
	info, err := r.managementInfo(ctx)
	if err != nil {
		return true, err
	}
	if method == operatorDescribeMethod {
		data, err := json.Marshal(info)
		if err != nil {
			return true, err
		}
		reply := rawMessage(data)
		return true, stream.SendMsg(&reply)
	}
	address := info.Address
	if method == operatorTCPMethod {
		var open operatorTCPRequest
		if len(request) > 256 || json.Unmarshal(request, &open) != nil || open.Port < 1 || open.Port > 65535 {
			return true, status.Error(codes.InvalidArgument, "mesh TCP open requires a destination port")
		}
		address, err = r.auxiliaryAddress(ctx, info, open.Port)
		if err != nil {
			return true, err
		}
	} else if info.Platform != "Lite" || info.Transport != "mtls-v1" {
		return true, status.Error(codes.Unimplemented, "target does not advertise native Lite WendyCom TLS")
	}
	r.mu.Lock()
	if r.closed || r.liteStreams >= operatorLiteStreams {
		r.mu.Unlock()
		return true, status.Error(codes.ResourceExhausted, "mesh TCP stream capacity is busy")
	}
	r.liteStreams++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.liteStreams--; r.mu.Unlock() }()
	dial, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := r.packetDial(dial, "tcp", address)
	cancel()
	if err != nil {
		return true, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	ready := rawMessage{}
	if err = stream.SendMsg(&ready); err != nil {
		return true, err
	}
	return true, relayOperatorLite(stream, conn)
}

func relayOperatorLite(stream grpc.ServerStream, conn net.Conn) error {
	input, output := make(chan error, 1), make(chan error, 1)
	go func() {
		for {
			var bytes rawMessage
			err := stream.RecvMsg(&bytes)
			if errors.Is(err, io.EOF) {
				if half, ok := conn.(interface{ CloseWrite() error }); ok {
					err = half.CloseWrite()
				} else {
					err = nil
				}
				input <- err
				return
			}
			if err != nil {
				input <- err
				return
			}
			if len(bytes) == 0 || len(bytes) > operatorLiteChunk {
				input <- status.Error(codes.InvalidArgument, "invalid mesh Lite byte frame")
				return
			}
			for len(bytes) > 0 {
				n, err := conn.Write(bytes)
				if err != nil {
					input <- err
					return
				}
				if n == 0 {
					input <- io.ErrShortWrite
					return
				}
				bytes = bytes[n:]
			}
		}
	}()
	go func() {
		buffer := make([]byte, operatorLiteChunk)
		for {
			n, err := conn.Read(buffer)
			if n > 0 {
				message := rawMessage(append([]byte(nil), buffer[:n]...))
				if send := stream.SendMsg(&message); send != nil {
					output <- send
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				output <- err
				return
			}
		}
	}()
	select {
	case err := <-input:
		if err != nil {
			return err
		}
		// Request half-close permits the peer to finish its response.
		select {
		case err := <-output:
			return err
		case <-stream.Context().Done():
			return stream.Context().Err()
		}
	case err := <-output:
		return err
	case <-stream.Context().Done():
		return stream.Context().Err()
	}
}

// operatorControl reuses the selected profile's adjacent attachment and probes
// that WendyOS peer, without sending a gRPC probe to a Lite destination.
func operatorControl(ctx context.Context, selector string) (*grpcclient.AgentConnection, error) {
	target, matched, err := ParseMeshSelector(selector)
	if err != nil || !matched {
		return nil, errors.New("invalid mesh selector")
	}
	certificate, operator, err := configuredOperator()
	if err != nil {
		return nil, err
	}
	if target.Tenant() != operator.Tenant() {
		return nil, errors.New("mesh target differs from the selected operator tenant")
	}
	directory, err := configDir()
	if err != nil {
		return nil, err
	}
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, operator.Principal()))
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		return PrepareOperatorAutomatic(ctx, certificate)
	}
	spec, err := operatorSpec(recipe, certificate)
	if err != nil {
		return nil, err
	}
	return connectOrStartOperator(ctx, spec, &certificate)
}

func DescribeOperatorTarget(ctx context.Context, selector string) (OperatorTargetInfo, error) {
	client, err := operatorControl(ctx, selector)
	if err != nil {
		return OperatorTargetInfo{}, err
	}
	defer client.Close()
	ctx = metadata.AppendToOutgoingContext(ctx, operatorTargetHeader, selector)
	request, reply := rawMessage{}, rawMessage{}
	if err = client.Conn.Invoke(ctx, operatorDescribeMethod, &request, &reply, grpc.ForceCodec(rawCodec{})); err != nil {
		return OperatorTargetInfo{}, err
	}
	var info OperatorTargetInfo
	if len(reply) > operatorLiteChunk || json.Unmarshal(reply, &info) != nil || info.Selector != selector {
		return info, errors.New("invalid operator target description")
	}
	return info, nil
}

// DialOperatorLite returns a caller-owned routed socket. Native WendyCom TLS
// runs end to end over it; this broker never terminates that destination TLS.
func DialOperatorLite(ctx context.Context, selector string) (net.Conn, error) {
	client, err := operatorControl(ctx, selector)
	if err != nil {
		return nil, err
	}
	conn, err := openOperatorLite(ctx, client.Conn, selector, client.Close)
	if err != nil {
		client.Close()
	}
	return conn, err
}

type operatorLiteConn struct {
	net.Conn
	remote               net.Conn
	stream               grpc.ClientStream
	cancel               context.CancelFunc
	release              func() error
	once                 sync.Once
	writeMu              sync.Mutex
	writeClosed          atomic.Bool
	writeDone            chan struct{}
	writeErr             error // published by closing writeDone
	closed               chan struct{}
	deadlineMu           sync.Mutex
	writeDeadline        time.Time
	writeDeadlineChanged chan struct{}
	readMu               sync.Mutex
	readErr              error
}

func (c *operatorLiteConn) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.cancel()
		c.Conn.Close()
		c.remote.Close()
		if c.release != nil {
			_ = c.release()
		}
	})
	return nil
}
func (c *operatorLiteConn) Read(bytes []byte) (int, error) {
	n, err := c.Conn.Read(bytes)
	if errors.Is(err, io.ErrClosedPipe) {
		c.readMu.Lock()
		terminal := c.readErr
		c.readMu.Unlock()
		if terminal != nil {
			err = terminal
		}
	}
	return n, err
}

func (c *operatorLiteConn) Write(bytes []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeClosed.Load() {
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(bytes)
}
func (c *operatorLiteConn) CloseWrite() error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if !c.writeClosed.Swap(true) {
		// Interrupt only the outgoing pipe reader; it finishes bytes accepted
		// by Write before closing the gRPC request direction exactly once.
		_ = c.remote.SetReadDeadline(time.Now())
	}
	for {
		select {
		case <-c.writeDone:
			return c.writeErr
		default:
		}
		c.deadlineMu.Lock()
		deadline := c.writeDeadline
		c.deadlineMu.Unlock()
		var timer *time.Timer
		var expired <-chan time.Time
		if !deadline.IsZero() {
			if !time.Now().Before(deadline) {
				// The final accepted pipe chunk can be parked in gRPC Send.
				// Retire this flow without closing the shared broker connection.
				_ = c.Close()
				return os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(time.Until(deadline))
			expired = timer.C
		}
		select {
		case <-c.writeDone:
			if timer != nil {
				timer.Stop()
			}
			return c.writeErr
		case <-c.closed:
			if timer != nil {
				timer.Stop()
			}
			return net.ErrClosed
		case <-c.writeDeadlineChanged:
		case <-expired:
		}
		if timer != nil {
			timer.Stop()
		}
		// A concurrent caller may have extended or cleared the deadline.
	}
}

func (c *operatorLiteConn) SetWriteDeadline(t time.Time) error {
	if err := c.Conn.SetWriteDeadline(t); err != nil && !c.writeClosed.Load() {
		return err
	}
	select {
	case <-c.closed:
		return net.ErrClosed
	default:
	}
	c.deadlineMu.Lock()
	c.writeDeadline = t
	c.deadlineMu.Unlock()
	select {
	case c.writeDeadlineChanged <- struct{}{}:
	default:
	}
	return nil
}

func (c *operatorLiteConn) SetDeadline(t time.Time) error {
	if err := c.Conn.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
func openOperatorLite(ctx context.Context, cc *grpc.ClientConn, selector string, release func() error) (net.Conn, error) {
	return openOperatorStream(ctx, cc, selector, operatorLiteMethod, nil, release)
}

func openOperatorStream(ctx context.Context, cc *grpc.ClientConn, selector, method string, request rawMessage, release func() error) (net.Conn, error) {
	owner, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	fail := func(err error) (net.Conn, error) { stop(); cancel(); return nil, err }
	owner = metadata.AppendToOutgoingContext(owner, operatorTargetHeader, selector)
	stream, err := cc.NewStream(owner, &grpc.StreamDesc{ClientStreams: true, ServerStreams: true}, method, grpc.ForceCodec(rawCodec{}), grpc.MaxCallRecvMsgSize(operatorLiteChunk), grpc.MaxCallSendMsgSize(operatorLiteChunk))
	if err != nil {
		return fail(err)
	}
	ready := rawMessage{}
	if err = stream.SendMsg(&request); err != nil {
		return fail(err)
	}
	if err = stream.RecvMsg(&ready); err != nil {
		return fail(err)
	}
	if len(ready) != 0 {
		return fail(errors.New("invalid mesh Lite stream ready"))
	}
	if !stop() && ctx.Err() != nil {
		return fail(ctx.Err())
	}
	local, remote := net.Pipe()
	conn := &operatorLiteConn{Conn: local, remote: remote, stream: stream, cancel: cancel, release: release, writeDone: make(chan struct{}), closed: make(chan struct{}), writeDeadlineChanged: make(chan struct{}, 1)}
	context.AfterFunc(owner, func() { _ = conn.Close() })
	go func() {
		defer conn.Close()
		for {
			var data rawMessage
			if err := stream.RecvMsg(&data); err != nil {
				conn.readMu.Lock()
				conn.readErr = err
				conn.readMu.Unlock()
				return
			}
			if len(data) == 0 || len(data) > operatorLiteChunk {
				return
			}
			if _, err := remote.Write(data); err != nil {
				return
			}
		}
	}()
	go func() {
		defer close(conn.writeDone)
		defer func() {
			if err := stream.CloseSend(); conn.writeErr == nil {
				conn.writeErr = err
			}
		}()
		buffer := make([]byte, operatorLiteChunk)
		for {
			n, err := remote.Read(buffer)
			if n > 0 {
				data := rawMessage(append([]byte(nil), buffer[:n]...))
				if err := stream.SendMsg(&data); err != nil {
					conn.writeErr = err
					conn.Close()
					return
				}
			}
			if err != nil {
				if !conn.writeClosed.Load() {
					conn.writeErr = err
					conn.Close()
				}
				return
			}
		}
	}()
	return conn, nil
}
