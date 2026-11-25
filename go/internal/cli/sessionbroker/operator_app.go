package sessionbroker

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshview"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const operatorAppMethod = "/wendy.sessionbroker.AppV1/Open"

func (r *operatorTargets) appServiceTopology(ctx context.Context, target meshid.Device) (*pb.LocalMeshTopology, error) {
	if r.localSnapshot == nil {
		return r.serviceTopology(ctx) // Coherent in-process fixtures only.
	}
	snapshot := r.localSnapshot()
	// Public inventory deliberately truncates a large mesh. Selecting an
	// already retained target must not spend its service budget on other owners.
	selected := make([]localmesh.Manifest, 0, 1)
	for _, manifest := range snapshot.Devices {
		owner, err := manifest.Peer(snapshot.Self.Tenant())
		if err == nil && owner == target {
			selected = append(selected, manifest)
		}
	}
	snapshot.Devices = selected
	return meshview.Topology(snapshot.Self, snapshot, time.Now(), true), nil
}

// DialOperatorApp opens a declared Lite TCP service through the retained Node.
// The broker verifies destination TLS and ordinary WAS1 running-app admission;
// the public manifest only selects the service and never replaces that check.
func DialOperatorApp(ctx context.Context, selector string, port uint16) (net.Conn, error) {
	if port == 0 {
		return nil, status.Error(codes.InvalidArgument, "mesh app port must be nonzero")
	}
	client, err := operatorControl(ctx, selector)
	if err != nil {
		return nil, err
	}
	conn, err := openOperatorApp(ctx, client.Conn, selector, port, client.Close)
	if err != nil {
		_ = client.Close()
	}
	return conn, err
}

func openOperatorApp(ctx context.Context, cc *grpc.ClientConn, selector string, port uint16, release func() error) (net.Conn, error) {
	if port == 0 {
		return nil, status.Error(codes.InvalidArgument, "mesh app port must be nonzero")
	}
	var request [2]byte
	binary.BigEndian.PutUint16(request[:], port)
	return openOperatorStream(ctx, cc, selector, operatorAppMethod, request[:], release)
}

func (r *operatorTargets) localApp(stream grpc.ServerStream, a *activity) (bool, error) {
	method, _ := grpc.MethodFromServerStream(stream)
	if method != operatorAppMethod {
		return false, nil
	}
	a.activeRPCs.Add(1)
	a.touch()
	defer func() { a.activeRPCs.Add(-1); a.touch() }()
	var request rawMessage
	if err := stream.RecvMsg(&request); err != nil {
		return true, err
	}
	if len(request) != 2 || binary.BigEndian.Uint16(request) == 0 {
		return true, status.Error(codes.InvalidArgument, "mesh app open requires a two-byte port")
	}
	port := binary.BigEndian.Uint16(request)
	info, err := r.managementInfo(stream.Context())
	if err != nil {
		return true, err
	}
	if info.Platform != "Lite" || r.appDial == nil {
		return true, status.Error(codes.Unimplemented, "native mesh app forwarding requires a Lite target")
	}
	peer, _, err := ParseMeshSelector(info.Selector)
	if err != nil {
		return true, err
	}
	view, err := r.appServiceTopology(stream.Context(), peer)
	if err != nil {
		return true, err
	}
	reporter, err := r.reporter()
	if err != nil {
		return true, err
	}
	declared := false
	if view != nil && view.ServicesIncluded {
		for _, hint := range view.Announcements {
			if hint.Platform != "Lite" || !serviceOriginLive(view, reporter, peer, hint, time.Now()) {
				continue
			}
			for _, service := range hint.Services {
				if service.Port == uint32(port) && strings.HasSuffix(service.Type, "._tcp") {
					declared = true
					break
				}
			}
		}
	}
	if !declared {
		return true, status.Error(codes.Unavailable, "Lite target has no currently routed TCP service on that port")
	}
	// Native management, auxiliary sockets and app TLS share the same bounded
	// admission. Pending TLS therefore cannot build an independent work queue.
	r.mu.Lock()
	if r.closed || r.liteStreams >= operatorLiteStreams {
		r.mu.Unlock()
		return true, status.Error(codes.ResourceExhausted, "mesh TCP stream capacity is busy")
	}
	r.liteStreams++
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.liteStreams--; r.mu.Unlock() }()
	setup, cancel := context.WithTimeout(stream.Context(), meshsession.TLSAppSessionSetupTimeout)
	conn, err := r.appDial(setup, peer, port)
	cancel()
	if err != nil {
		return true, err
	}
	defer conn.Close()
	stop := context.AfterFunc(stream.Context(), func() { _ = conn.Close() })
	defer stop()
	ready := rawMessage{}
	if err = stream.SendMsg(&ready); err != nil {
		return true, err
	}
	return true, relayOperatorLite(stream, conn)
}
