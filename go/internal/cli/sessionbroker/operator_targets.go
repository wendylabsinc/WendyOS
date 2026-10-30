package sessionbroker

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshview"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const operatorTargetHeader = "wendy-operator-target"

// Each client has 256 KiB read/write buffers and independent HTTP/2 windows.
const operatorRemoteClients = 16

type operatorTarget struct {
	client *grpcclient.AgentConnection
	ready  chan struct{}
	err    error
	active int
	used   time.Time
}
type operatorTargets struct {
	localSnapshot   func() localmesh.NodeSnapshot
	mu              sync.Mutex
	pins            profilePins
	adjacent        *grpc.ClientConn
	currentAdjacent func() (*grpc.ClientConn, meshid.Device, error)
	certificate     config.CertificateInfo
	spec            Spec
	entries         map[string]*operatorTarget
	closed          bool
	topology        func(context.Context) (*pb.LocalMeshTopology, error)
	targetTopology  func(context.Context, meshid.Device) (*pb.LocalMeshTopology, error)
	serviceTopology func(context.Context) (*pb.LocalMeshTopology, error)
	dial            func(context.Context, string, *config.CertificateInfo, meshid.Device) (*grpcclient.AgentConnection, error)
	packetDial      func(context.Context, string, string) (net.Conn, error)
	appDial         func(context.Context, meshid.Device, uint16) (net.Conn, error)
	liteStreams     int
	directory       directoryPublisher
}

// Reload before every handshake, including gRPC reconnects: a retained helper
// must not use a startup pin snapshot after another invocation changes it.
type profilePins struct {
	mu        sync.Mutex
	directory string
}

func (p *profilePins) CheckAndUpdate(leaf *x509.Certificate, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	store, err := devicepin.Open(p.directory)
	if err != nil {
		return err
	}
	return store.CheckAndUpdate(leaf, name)
}
func newOperatorTargets(session *operatormesh.Session, adjacent *grpc.ClientConn, certificate config.CertificateInfo, directory string, spec Spec) *operatorTargets {
	r := &operatorTargets{adjacent: adjacent, certificate: certificate, spec: spec, entries: make(map[string]*operatorTarget), pins: profilePins{directory: directory}, packetDial: session.DialContext}
	// The broker owns the shared Node. Fixtures may inject a coherent local
	// view; production never mirrors a neighbour through a second directory.
	r.topology = func(context.Context) (*pb.LocalMeshTopology, error) {
		return nil, status.Error(codes.Unavailable, "local mesh participant is unavailable")
	}
	r.targetTopology = func(ctx context.Context, _ meshid.Device) (*pb.LocalMeshTopology, error) { return r.topology(ctx) }
	r.serviceTopology = r.topology
	if session != nil && session.Node != nil {
		r.appDial = session.DialAppTLS
		r.localSnapshot = session.Snapshot
		r.topology = func(context.Context) (*pb.LocalMeshTopology, error) {
			snapshot := r.localSnapshot()
			return meshview.Topology(snapshot.Self, snapshot, time.Now()), nil
		}
		r.targetTopology = func(ctx context.Context, _ meshid.Device) (*pb.LocalMeshTopology, error) { return r.topology(ctx) }
		r.serviceTopology = func(context.Context) (*pb.LocalMeshTopology, error) {
			snapshot := r.localSnapshot()
			return meshview.Topology(snapshot.Self, snapshot, time.Now(), true), nil
		}
	}
	r.dial = func(ctx context.Context, address string, certificate *config.CertificateInfo, target meshid.Device) (*grpcclient.AgentConnection, error) {
		expected := operatorExpected(target)
		return grpcclient.ConnectWithTLSExpecting(ctx, address, certificate, &r.pins, &expected, grpc.WithIdleTimeout(DefaultIdleTTL), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			current, err := r.endpoint(ctx, target)
			if err != nil {
				return nil, err
			}
			if current != address {
				return nil, fmt.Errorf("mesh agent endpoint changed; resolve the selector again")
			}
			return r.packetDial(ctx, "tcp", address)
		}))
	}
	r.directory.owner = r
	return r
}
func (r *operatorTargets) adjacentPeer() (*grpc.ClientConn, meshid.Device, error) {
	if r.currentAdjacent != nil {
		return r.currentAdjacent()
	}
	operator, err := operatorPrincipal(r.certificate)
	if err != nil {
		return nil, meshid.Device{}, err
	}
	peer, err := meshid.ParseDevice(operator.Tenant(), r.spec.Operator.RouterUUID)
	return r.adjacent, peer, err
}

func (r *operatorTargets) lookupTopology(ctx context.Context, target meshid.Device) (*pb.LocalMeshTopology, error) {
	if r.targetTopology != nil {
		return r.targetTopology(ctx, target)
	}
	return r.topology(ctx) // Coherent in-process fixture/native-source injection.
}

func (r *operatorTargets) endpoint(ctx context.Context, target meshid.Device) (string, error) {
	view, err := r.lookupTopology(ctx, target)
	if err != nil {
		return "", fmt.Errorf("read attached router topology: %w", err)
	}
	adjacent, err := r.reporter()
	if err != nil {
		return "", err
	}
	return operatorAgentEndpoint(view, adjacent, target, time.Now())
}

// These are this exact authenticated adjacent node's fresh observations, not
// portable proofs. They select a numeric endpoint, never its TLS identity.
func operatorAgentEndpoint(view *pb.LocalMeshTopology, adjacent, target meshid.Device, now time.Time) (string, error) {
	info, err := operatorManagementTarget(view, adjacent, target, now)
	if err != nil {
		return "", err
	}
	if info.Platform != "WendyOS" {
		return "", status.Errorf(codes.Unimplemented, "mesh target speaks %s management; select its Lite provider", info.Platform)
	}
	return info.Address, nil
}

func operatorManagementTarget(view *pb.LocalMeshTopology, adjacent, target meshid.Device, now time.Time) (OperatorTargetInfo, error) {
	if view == nil || !view.RuntimeAvailable || view.SelfPrincipal != adjacent.Principal() || view.SelfTenantId != target.Tenant().String() || view.SelfDeviceId != adjacent.UUID().String() || view.SelfMeshAddress != adjacent.MeshAddress().String() {
		return OperatorTargetInfo{}, status.Error(codes.Unavailable, "attached router has no current mesh observation")
	}
	captured := time.UnixMilli(view.CapturedAtUnixMs)
	if captured.After(now.Add(time.Second)) || now.Sub(captured) > 10*time.Second {
		return OperatorTargetInfo{}, status.Error(codes.Unavailable, "attached router mesh observation is stale")
	}
	prefix := netip.PrefixFrom(target.MeshAddress(), 128).String()
	for _, hint := range view.Announcements {
		if hint.Principal != target.Principal() || hint.DeviceId != target.UUID().String() || hint.Prefix != prefix || hint.TrustState != "verified-at-snapshot" || hint.ExpiresAtUnixMs <= now.UnixMilli() {
			continue
		}
		live := false
		for _, route := range view.Routes {
			if route.Prefix == prefix && route.OriginPrincipal == target.Principal() && route.OriginRouterId == hint.OriginRouterId && route.SequenceNumber == hint.SequenceNumber && !route.Unreachable && (!route.Local || target == adjacent) && route.OriginAuthorized && route.SignedRouteExpiresAtUnixMs == hint.ExpiresAtUnixMs {
				live = true
				break
			}
		}
		if !live {
			continue
		}
		if (hint.Platform != "WendyOS" && hint.Platform != "Lite") || hint.AgentTransport != "mtls-v1" {
			return OperatorTargetInfo{}, status.Errorf(codes.Unimplemented, "mesh management adapter for platform %q transport %q is not implemented", hint.Platform, hint.AgentTransport)
		}
		if hint.AgentPort == 0 || hint.AgentPort > 65535 {
			return OperatorTargetInfo{}, status.Error(codes.Unavailable, "mesh agent has no usable advertised port")
		}
		return OperatorTargetInfo{Selector: MeshSelector(target), Principal: target.Principal(),
			Tenant: target.Tenant().String(), Device: target.UUID().String(), Name: hint.Name,
			Expires: hint.ExpiresAtUnixMs, Revision: hint.Revision, Platform: hint.Platform, Transport: hint.AgentTransport, Caps: append([]string(nil), hint.Caps...),
			Address: net.JoinHostPort(target.MeshAddress().String(), strconv.Itoa(int(hint.AgentPort)))}, nil
	}
	return OperatorTargetInfo{}, status.Error(codes.Unavailable, "mesh target has no live authorized route and agent announcement")
}
func (r *operatorTargets) Resolve(ctx context.Context) (*grpc.ClientConn, func(), error) {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(operatorTargetHeader)
	if len(values) == 0 {
		values = []string{r.spec.Host}
	}
	if len(values) != 1 {
		return nil, nil, status.Error(codes.InvalidArgument, "one mesh target is required")
	}
	target, matched, err := ParseMeshSelector(values[0])
	if err != nil || !matched {
		return nil, nil, status.Error(codes.InvalidArgument, "invalid mesh target")
	}
	operator, err := operatorPrincipal(r.certificate)
	if err != nil || target.Tenant() != operator.Tenant() {
		return nil, nil, status.Error(codes.InvalidArgument, "mesh target differs from the selected operator tenant")
	}
	adjacentConn, adjacent, err := r.adjacentPeer()
	if err != nil && r.localSnapshot == nil {
		return nil, nil, err
	}
	if err == nil && target == adjacent && adjacentConn != nil {
		return adjacentConn, func() {}, nil
	}
	address, err := r.endpoint(ctx, target)
	if err != nil {
		return nil, nil, err
	}
	key := target.Principal() + "\x00" + address
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, nil, status.Error(codes.Unavailable, "operator attachment is closing")
	}
	entry := r.entries[key]
	if entry == nil {
		// Never retire a client borrowed by an active RPC when an endpoint moves.
		if len(r.entries) >= operatorRemoteClients {
			var oldest string
			for k, e := range r.entries {
				if e.active == 0 && (oldest == "" || e.used.Before(r.entries[oldest].used)) {
					oldest = k
				}
			}
			if oldest == "" {
				r.mu.Unlock()
				return nil, nil, status.Error(codes.ResourceExhausted, "all retained mesh agent clients are busy")
			}
			old := r.entries[oldest]
			delete(r.entries, oldest)
			if old.client != nil {
				_ = old.client.Close()
			}
		}
		entry = &operatorTarget{ready: make(chan struct{}), active: 1, used: time.Now()}
		r.entries[key] = entry
		r.mu.Unlock()
		client, dialErr := r.dial(ctx, address, &r.certificate, target)
		if dialErr == nil {
			probe, stop := context.WithTimeout(ctx, 15*time.Second)
			_, dialErr = client.AgentService.GetAgentVersion(probe, &agentpb.GetAgentVersionRequest{})
			stop()
		}
		r.mu.Lock()
		entry.client, entry.err = client, dialErr
		close(entry.ready)
		if r.closed && client != nil {
			_ = client.Close()
		}
		r.mu.Unlock()
	} else {
		entry.active++
		r.mu.Unlock()
	}
	release := func() {
		r.mu.Lock()
		entry.active--
		entry.used = time.Now()
		if entry.active == 0 && (entry.err != nil || r.closed) {
			delete(r.entries, key)
			if entry.client != nil {
				_ = entry.client.Close()
			}
		}
		r.mu.Unlock()
	}
	select {
	case <-ctx.Done():
		release()
		return nil, nil, ctx.Err()
	case <-entry.ready:
	}
	if entry.err != nil {
		release()
		return nil, nil, entry.err
	}
	return entry.client.Conn, release, nil
}
func (r *operatorTargets) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	r.directory.close()
	for key, entry := range r.entries {
		if entry.client != nil {
			_ = entry.client.Close()
		}
		if entry.active == 0 {
			delete(r.entries, key)
		}
	}
}

// reporter is the local full participant in production. Adjacent observations
// remain an explicit host-fixture/observer source, never local route authority.
func (r *operatorTargets) reporter() (meshid.Device, error) {
	if r.localSnapshot != nil {
		snapshot := r.localSnapshot()
		if !snapshot.Self.IsValid() {
			return meshid.Device{}, status.Error(codes.Unavailable, "local mesh participant is starting")
		}
		return snapshot.Self, nil
	}
	_, peer, err := r.adjacentPeer()
	return peer, err
}
