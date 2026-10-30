// Package litetopology normalizes bounded WendyCom observations without
// depending on serial ports, radios, discovery, or an operator mesh participant.
package litetopology

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	lite "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type ReadPage func(context.Context, *lite.WendyComNetTopologyParams) (*lite.WendyComNetTopologyPage, error)

// Read collects serial pages within a fixed record/byte budget. A structural
// change discards all collected rows and permits one fresh attempt. Periodic
// routing timers do not change the device's structural generation.
func Read(ctx context.Context, read ReadPage) (*pb.LocalMeshTopology, error) {
	return readWithin(ctx, read, AggregateBudget)
}
func readWithin(parent context.Context, read ReadPage, budget time.Duration) (*pb.LocalMeshTopology, error) {
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		snapshot, changed, err := collect(ctx, read)
		if err != nil {
			return nil, err
		}
		if !changed {
			return snapshot, nil
		}
	}
	return nil, status.Error(codes.Unavailable, "topology changed during pagination; retry shortly")
}
func collect(ctx context.Context, read ReadPage) (*pb.LocalMeshTopology, bool, error) {
	var first *lite.WendyComNetTopologyPage
	var out *pb.LocalMeshTopology
	request := &lite.WendyComNetTopologyParams{}
	total := 0
	for count := 0; count < 1024; count++ {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		page, err := read(ctx, request)
		if status.Code(err) == codes.Aborted {
			return nil, true, nil
		}
		if err != nil {
			return nil, false, err
		}
		if page == nil {
			return nil, false, status.Error(codes.Unimplemented, "device does not support topology observations")
		}
		total += proto.Size(page)
		if total > 512*1024 {
			return nil, false, status.Error(codes.ResourceExhausted, "Lite topology exceeds observation byte budget")
		}
		if first == nil {
			first = page
			tenant, e := uuid.FromBytes(page.Tenant)
			if e != nil {
				return nil, false, e
			}
			device, e := uuid.FromBytes(page.Device)
			if e != nil {
				return nil, false, e
			}
			principal := fmt.Sprintf("spiffe://wendy.sh/tenant/%s/device/%s", tenant, device)
			identity, e := meshid.DeviceFromPrincipal(principal)
			if e != nil {
				return nil, false, e
			}
			out = &pb.LocalMeshTopology{SelfPrincipal: principal, SelfTenantId: tenant.String(), SelfDeviceId: device.String(), SelfMeshAddress: identity.MeshAddress().String(), CapturedAtUnixMs: page.CapturedAtUnixMs, RuntimeAvailable: page.RuntimeAvailable, Status: normalizeStatus(page)}
		} else if page.Generation != first.Generation || !bytes.Equal(page.Tenant, first.Tenant) || !bytes.Equal(page.Device, first.Device) || page.RuntimeAvailable != first.RuntimeAvailable || page.Partial != first.Partial || !proto.Equal(page.GetStatus().GetDesired(), first.GetStatus().GetDesired()) || page.GetStatus().GetState() != first.GetStatus().GetState() {
			return nil, true, nil
		}
		out.Truncated = out.Truncated || page.Truncated || page.Partial
		if page.RuntimeAvailable && page.Record != nil {
			if err := appendRecord(out, page.Record); err != nil {
				return nil, false, err
			}
		}
		if page.NextCursor == 0 {
			return out, false, nil
		}
		if page.Generation == 0 || page.NextCursor <= request.Cursor || page.NextCursor > 1024 {
			return nil, false, status.Error(codes.DataLoss, "invalid Lite topology cursor")
		}
		request = &lite.WendyComNetTopologyParams{Cursor: page.NextCursor, Generation: page.Generation}
	}
	return nil, false, status.Error(codes.ResourceExhausted, "Lite topology exceeds observation record budget")
}
func normalizeStatus(page *lite.WendyComNetTopologyPage) *pb.LocalMeshStatus {
	net := page.GetStatus()
	config := net.GetDesired()
	state := "runtime-unavailable"
	if page.RuntimeAvailable {
		state = "mesh-active"
	}
	if net != nil {
		switch net.GetState() {
		case lite.WendyComNetState_WENDY_COM_NET_OFF:
			state = "disabled"
		case lite.WendyComNetState_WENDY_COM_NET_STARTING:
			state = "starting"
		case lite.WendyComNetState_WENDY_COM_NET_RUNNING:
			if page.RuntimeAvailable {
				state = "mesh-active"
			} else {
				state = "waiting-for-peers"
			}
		case lite.WendyComNetState_WENDY_COM_NET_STOPPING:
			state = "stopping"
		case lite.WendyComNetState_WENDY_COM_NET_FAILED:
			state = "failed"
		}
	}
	detail := "Wendy Lite local observation."
	if page.Partial {
		detail += " Partial endpoint directory; remote routes and full signed origin details are not retained on this device."
	}
	if net.GetRestarting() {
		detail += " Saved radio change is awaiting restart."
	}
	var configured *pb.LocalMeshConfiguration
	if config != nil {
		configured = &pb.LocalMeshConfiguration{Participate: config.GetEnabled(), Ble: config.GetBle(), Nan: config.GetNan(), InfrastructureWifi: config.GetInfrastructureWifi()}
	}
	return &pb.LocalMeshStatus{RuntimeAvailable: page.RuntimeAvailable, State: state, Detail: detail, AuthenticatedPeers: net.GetActiveLinks(), Configured: configured}
}
func principal(tenant string, raw []byte) (string, string, error) {
	if len(raw) == 0 {
		return "", "", nil
	}
	id, err := uuid.FromBytes(raw)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprintf("spiffe://wendy.sh/tenant/%s/device/%s", tenant, id), id.String(), nil
}
func address(raw []byte) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	if len(raw) != 16 {
		return "", fmt.Errorf("invalid Lite IPv6 address")
	}
	var a [16]byte
	copy(a[:], raw)
	return netip.AddrFrom16(a).String(), nil
}

// The wire installation is bytes16, but normalized protobuf fields are UUID
// text. Never marshal the generated WendyCom record directly into UI JSON.
func owner(tenant, addr string, row *lite.WendyComNetTopologyRecord) (string, string, error) {
	switch row.OwnerRole {
	case 0, 1:
		if row.OwnerPrincipal != "" || len(row.OwnerInstance) != 0 {
			return "", "", status.Error(codes.DataLoss, "Lite device row contains operator identity fields")
		}
		return principal(tenant, row.Device)
	case 2:
		operator, err := meshoperator.ParsePrincipal(row.OwnerPrincipal)
		if err != nil || operator.Tenant().String() != tenant || len(row.Device) != 0 {
			return "", "", status.Error(codes.DataLoss, "Lite operator row has no matching tenant identity")
		}
		instance, err := uuid.FromBytes(row.OwnerInstance)
		if err != nil || instance == uuid.Nil {
			return "", "", status.Error(codes.DataLoss, "Lite operator row has no installation UUID")
		}
		endpoint := meshoperator.Endpoint{Operator: operator, Instance: meshoperator.InstanceID(instance)}
		if endpoint.Address().String() != addr {
			return "", "", status.Error(codes.DataLoss, "Lite operator row address does not match its installation")
		}
		return operator.Principal(), instance.String(), nil
	default:
		return "", "", status.Error(codes.DataLoss, "unknown Lite topology owner role")
	}
}
func appendRecord(out *pb.LocalMeshTopology, row *lite.WendyComNetTopologyRecord) error {
	addr, err := address(row.Address)
	if err != nil {
		return err
	}
	origin, id, err := owner(out.SelfTenantId, addr, row)
	if err != nil {
		return err
	}
	switch row.Kind {
	case 1:
		if origin == "" || addr == "" {
			return status.Error(codes.DataLoss, "incomplete Lite peer")
		}
		transport := map[uint32]string{1: "infrastructure-wifi", 2: "ble", 3: "nan"}[row.Transport]
		out.Peers = append(out.Peers, &pb.LocalMeshPeerLink{LinkId: row.LinkId, Principal: origin, DeviceId: id, MeshAddress: addr, Transport: transport, Cost: row.Metric})
	case 2:
		if addr == "" {
			return status.Error(codes.DataLoss, "incomplete Lite route")
		}
		next, _, err := principal(out.SelfTenantId, row.NextHopDevice)
		if err != nil {
			return err
		}
		nextAddress := ""
		if next != "" {
			d, e := meshid.DeviceFromPrincipal(next)
			if e != nil {
				return e
			}
			nextAddress = d.MeshAddress().String()
		}
		out.Routes = append(out.Routes, &pb.LocalMeshRoute{Prefix: addr + "/128", OriginRouterId: row.RouterId, SequenceNumber: row.Sequence, Metric: row.Metric, LinkId: row.LinkId, Local: row.Local, Unreachable: row.Unreachable, OriginPrincipal: origin, OriginAuthorized: row.Authorized && origin != "", SignedRouteExpiresAtUnixMs: row.ExpiresAtUnixMs, NextHopPrincipal: next, NextHopAddress: nextAddress})
	case 3:
		if origin == "" || addr == "" {
			return status.Error(codes.DataLoss, "incomplete Lite announcement")
		}
		trust := ""
		if row.Authorized {
			trust = "verified-at-snapshot"
		}
		out.Announcements = append(out.Announcements, &pb.LocalMeshAnnouncement{Principal: origin, DeviceId: id, Prefix: addr + "/128", OriginRouterId: row.RouterId, SequenceNumber: row.Sequence, Revision: row.Revision, IssuedAtUnixMs: row.IssuedAtUnixMs, ExpiresAtUnixMs: row.ExpiresAtUnixMs, TrustState: trust})
	default:
		return status.Error(codes.DataLoss, "unknown Lite topology record")
	}
	return nil
}
