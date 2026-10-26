package services

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/meshingress"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// meshDisabledFile, when present in the agent config dir, turns off the
// device side of the mesh (LAN MeshDial). Mesh defaults to enabled, matching
// the cloud org flag's default; the cloud phase will sync the org flag into
// this file.
const meshDisabledFile = "mesh-disabled"

// MeshService is the serving side of the LAN-direct mesh path: a peer agent
// opens MeshDial over this device's mTLS port and the stream carries one TCP
// connection to a local port. The cloud-relay serving path needs no service —
// it reuses the existing tunnel broker DialRequest handling.
type MeshService struct {
	agentpbv2.UnimplementedWendyMeshServiceServer
	logger           *zap.Logger
	meshDisabledPath string
	// expectedScope is the current device's tenant. Unknown or legacy scopes
	// cannot authorize mesh access, even if the general mTLS interceptor is off.
	expectedScope certs.Scope
	dialLocal     func(addr string, timeout time.Duration) (net.Conn, error) // swapped in tests
	ingress       interface {
		DialAuthorized(uint16, func(string) (net.Conn, error)) (net.Conn, error)
	}
}

// SetIngressAuthorizer restricts the legacy LAN MeshDial path to ports owned
// by live mesh-mode apps. A missing authorizer denies every port.
func (s *MeshService) SetIngressAuthorizer(authorizer interface {
	DialAuthorized(uint16, func(string) (net.Conn, error)) (net.Conn, error)
}) {
	s.ingress = authorizer
}

// NewMeshService builds the serving side of the LAN-direct mesh path.
// An unknown or legacy expectedScope denies mesh calls.
func NewMeshService(logger *zap.Logger, configPath string, expectedScope certs.Scope) *MeshService {
	return &MeshService{
		logger:           logger,
		meshDisabledPath: filepath.Join(configPath, meshDisabledFile),
		expectedScope:    expectedScope,
		dialLocal: func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("tcp", addr, timeout)
		},
	}
}

func (s *MeshService) MeshDial(stream agentpbv2.WendyMeshService_MeshDialServer) error {
	ident, err := s.deviceIdentityFromContext(stream.Context())
	if err != nil {
		return err
	}
	if s.meshDisabled() {
		return status.Error(codes.PermissionDenied, "wendy net is disabled on this device")
	}
	first, err := stream.Recv()
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "reading open message: %v", err)
	}
	open := first.GetOpen()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first MeshDial message must be open")
	}
	if open.Port == 0 || open.Port > 65535 {
		return status.Errorf(codes.InvalidArgument, "invalid port %d", open.Port)
	}
	if s.ingress == nil {
		return status.Error(codes.PermissionDenied, "wendy net ingress port is not published by a running app")
	}
	// Dial only the verified endpoint of the admitted running container.
	conn, err := s.ingress.DialAuthorized(uint16(open.Port), func(address string) (net.Conn, error) {
		return s.dialLocal(address, 10*time.Second)
	})
	if err != nil {
		if errors.Is(err, meshingress.ErrPortDenied) {
			return status.Error(codes.PermissionDenied, "wendy net ingress port is not published by a running app")
		}
		return status.Errorf(codes.Unavailable, "dialing local port %d: %v", open.Port, err)
	}
	defer conn.Close()
	s.logger.Info("wendy net dial accepted",
		zap.String("caller_principal", ident.Principal()),
		zap.Uint32("port", open.Port))
	return s.relay(stream, conn)
}

// deviceIdentityFromContext independently enforces a canonical device
// principal in this device's tenant. The mTLS listener verifies certificate
// trust; disabling its general tenant interceptor cannot relax mesh admission.
func (s *MeshService) deviceIdentityFromContext(ctx context.Context) (meshid.Device, error) {
	tenant, err := meshid.ParseTenant(s.expectedScope.TenantUUID)
	if err != nil {
		return meshid.Device{}, status.Error(codes.PermissionDenied, "wendy net dial requires an enrolled local tenant")
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return meshid.Device{}, status.Error(codes.PermissionDenied, "wendy net dial requires mTLS")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return meshid.Device{}, status.Error(codes.PermissionDenied, "wendy net dial requires a client certificate")
	}
	device, err := meshid.DeviceFromCert(tlsInfo.State.PeerCertificates[0])
	if err != nil {
		return meshid.Device{}, status.Error(codes.PermissionDenied, "wendy net dial requires a canonical device principal")
	}
	if device.Tenant() != tenant {
		return meshid.Device{}, status.Error(codes.PermissionDenied, "wendy net dial requires a same-tenant device certificate")
	}
	return device, nil
}

func (s *MeshService) meshDisabled() bool {
	_, err := os.Stat(s.meshDisabledPath)
	return err == nil
}

// relay mirrors TunnelBrokerClient.relay (tunnel_broker_client.go:251) with
// MeshDial framing.
func (s *MeshService) relay(stream agentpbv2.WendyMeshService_MeshDialServer, conn net.Conn) error {
	// true means a hard transport failure; false is a graceful half-close. A
	// hard failure must return the handler immediately so gRPC cancels the
	// stream context, while closing conn unblocks the opposite local copy loop.
	// Waiting for two graceful completions is still required to avoid truncating
	// a response after one side half-closes.
	done := make(chan bool, 2)

	// Watcher: force-unblock a parked conn.Read when the stream dies. gRPC
	// cancels the stream context both when the client's stream/connection
	// dies abruptly and when this handler returns, so the local conn → stream
	// goroutine can never stay blocked in Read forever (which would leak the
	// goroutine, the local socket, and this handler). The graceful path
	// (Recv EOF → CloseWrite below) still lets in-flight responses drain
	// first; the extra Close on top of MeshDial's deferred one is harmless.
	go func() {
		<-stream.Context().Done()
		conn.Close()
	}()

	go func() { // stream → local conn
		for {
			msg, err := stream.Recv()
			if err != nil {
				// stream.Recv() returning an error (including io.EOF, which
				// per gRPC semantics means the peer called CloseSend) only
				// tells us the peer is done sending — it does not mean the
				// whole stream/connection is dead. Half-close the local
				// conn's write side, mirroring the explicit HalfClose case
				// below, so the local conn → stream goroutine can still
				// forward any in-flight response before the caller's
				// deferred conn.Close() runs once both directions finish.
				if tc, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = tc.CloseWrite()
				}
				done <- !errors.Is(err, io.EOF)
				return
			}
			d := msg.GetData()
			if d == nil {
				continue
			}
			if len(d.Payload) > 0 {
				if _, err := conn.Write(d.Payload); err != nil {
					_ = conn.Close()
					done <- true
					return
				}
			}
			if d.HalfClose {
				if tc, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = tc.CloseWrite()
				}
			}
		}
	}()

	go func() { // local conn → stream
		buf := make([]byte, 256*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				// Copy out of the shared read buffer before handing it to
				// Send, mirroring TunnelBrokerClient.relay
				// (tunnel_broker_client.go:286-288) — the buffer is reused on
				// the next loop iteration, and Send must not be assumed to
				// have finished consuming it synchronously.
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if sendErr := stream.Send(&agentpbv2.MeshDialData{Payload: payload}); sendErr != nil {
					_ = conn.Close()
					done <- true
					return
				}
			}
			if err != nil {
				hard := !errors.Is(err, io.EOF)
				if !hard {
					hard = stream.Send(&agentpbv2.MeshDialData{HalfClose: true}) != nil
				}
				if hard {
					_ = conn.Close()
				}
				done <- hard
				return
			}
		}
	}()

	for completed := 0; completed < 2; completed++ {
		if hard := <-done; hard {
			_ = conn.Close()
			return nil
		}
	}
	return nil
}
