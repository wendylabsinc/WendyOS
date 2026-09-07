package services

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
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
	// expectedScope is this device's own tenant, used to enforce that a MeshDial
	// caller belongs to the same tenant even when the server's mTLS
	// interceptor is disabled (WENDY_MTLS_ORG_ENFORCEMENT=off). An unknown
	// scope means the device could not identify itself; see
	// assetIdentityFromContext.
	expectedScope certs.Scope
	dialLocal     func(addr string, timeout time.Duration) (net.Conn, error) // swapped in tests
}

// NewMeshService builds the serving side of the LAN-direct mesh path.
// expectedScope is this device's own tenant (the same value passed to
// mtls.NewServer); pass a zero Scope when the device cannot identify itself
// (main.go forces mTLS enforcement off in that case too) — MeshService then
// skips its own-tenant check rather than reject every caller.
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
	ident, err := s.assetIdentityFromContext(stream.Context())
	if err != nil {
		return err
	}
	if s.meshDisabled() {
		return status.Error(codes.PermissionDenied, "mesh is disabled on this device")
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
	// Same SSRF stance as the broker path (tunnel_broker_client.go:207-213):
	// only local services are reachable.
	conn, err := s.dialLocal(net.JoinHostPort("127.0.0.1", strconv.Itoa(int(open.Port))), 10*time.Second)
	if err != nil {
		return status.Errorf(codes.Unavailable, "dialing local port %d: %v", open.Port, err)
	}
	defer conn.Close()
	s.logger.Info("mesh dial accepted",
		zap.Int32("caller_org", ident.OrgID),
		zap.String("caller_asset", ident.EntityID),
		zap.Uint32("port", open.Port))
	return s.relay(stream, conn)
}

// assetIdentityFromContext requires an mTLS peer whose leaf certificate
// carries a wendy asset identity belonging to this device's own org.
//
// Org equality is also enforced by the server's mandatory mTLS interceptors,
// but only when WENDY_MTLS_ORG_ENFORCEMENT is not "off" — an operator can
// disable that enforcement, and the design requires MeshDial to reject
// cross-org callers regardless (spec: "MeshDial rejects user certificates,
// cross-org asset certificates, and flag-off orgs with PermissionDenied").
// MeshService must therefore not delegate org equality to the interceptor;
// this method checks it directly. It also adds the asset-vs-user distinction
// the interceptors don't check.
func (s *MeshService) assetIdentityFromContext(ctx context.Context) (certs.WendyIdentity, error) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return certs.WendyIdentity{}, status.Error(codes.PermissionDenied, "mesh dial requires mTLS")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return certs.WendyIdentity{}, status.Error(codes.PermissionDenied, "mesh dial requires a client certificate")
	}
	ident, found, err := certs.IdentityFromCert(tlsInfo.State.PeerCertificates[0])
	if err != nil || !found {
		return certs.WendyIdentity{}, status.Error(codes.PermissionDenied, "client certificate carries no wendy identity")
	}
	if ident.EntityType != certs.EntityAsset {
		return certs.WendyIdentity{}, status.Error(codes.PermissionDenied, "mesh dial requires an asset certificate")
	}
	// An unknown expectedScope means this device could not identify itself
	// (main.go forces mTLS enforcement off for the same reason): there is
	// nothing meaningful to compare against, so skip the check rather than
	// reject every caller. Still requires the asset check above.
	if s.expectedScope.Known() && !ident.Scope().Matches(s.expectedScope) {
		return certs.WendyIdentity{}, status.Error(codes.PermissionDenied, "mesh dial requires a same-tenant asset certificate")
	}
	return ident, nil
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
				if tc, ok := conn.(*net.TCPConn); ok {
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
				if tc, ok := conn.(*net.TCPConn); ok {
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
