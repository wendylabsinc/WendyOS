package sessionbroker

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const operatorServicesMethod = "/wendy.sessionbroker.ServicesV1/List"
const operatorServicesBytes = 256 * 1024
const operatorServicesRows = 512

type OperatorServiceQuery struct {
	Mesh string `json:"mesh,omitempty"`
	Type string `json:"type,omitempty"`
}

type OperatorServiceInfo struct {
	Selector  string   `json:"selector"`
	Principal string   `json:"principal"`
	Name      string   `json:"name,omitempty"`
	Platform  string   `json:"platform,omitempty"`
	Mesh      string   `json:"mesh"`
	App       string   `json:"app"`
	Service   string   `json:"service"`
	Type      string   `json:"type"`
	Instance  string   `json:"instance"`
	Port      uint32   `json:"port"`
	TXT       []string `json:"txt,omitempty"`
	Expires   int64    `json:"expiresAtUnixMs"`
	Revision  uint64   `json:"revision,string"`
}

type OperatorServices struct {
	Version   int                   `json:"version"`
	State     string                `json:"state"`
	Source    string                `json:"source,omitempty"`
	Captured  int64                 `json:"capturedAtUnixMs,omitempty"`
	Truncated bool                  `json:"truncated,omitempty"`
	Services  []OperatorServiceInfo `json:"services"`
}

func (query OperatorServiceQuery) validate() error {
	if len(query.Mesh) > 63 || len(query.Type) > 63 || strings.ContainsAny(query.Mesh+query.Type, "\x00\r\n") {
		return status.Error(codes.InvalidArgument, "mesh service query exceeds its bounds")
	}
	return nil
}

func serviceOriginLive(view *pb.LocalMeshTopology, adjacent, device meshid.Device, hint *pb.LocalMeshAnnouncement, now time.Time) bool {
	prefix := netip.PrefixFrom(device.MeshAddress(), 128).String()
	if hint.Principal != device.Principal() || hint.DeviceId != device.UUID().String() || hint.Prefix != prefix || hint.TrustState != "verified-at-snapshot" || hint.ExpiresAtUnixMs <= now.UnixMilli() {
		return false
	}
	for _, route := range view.Routes {
		if route.Prefix == prefix && route.OriginPrincipal == device.Principal() && route.OriginRouterId == hint.OriginRouterId && route.SequenceNumber == hint.SequenceNumber && route.OriginAuthorized && !route.Unreachable && (!route.Local || device == adjacent) && route.SignedRouteExpiresAtUnixMs == hint.ExpiresAtUnixMs {
			return true
		}
	}
	return false
}

// serviceSnapshot reads public descriptions from the same current verified
// manifest and usable route. It has no service gossip/cache or refresh timer.
func (r *operatorTargets) serviceSnapshot(ctx context.Context, query OperatorServiceQuery) (OperatorServices, error) {
	out := OperatorServices{Version: 1, State: "unavailable", Services: []OperatorServiceInfo{}}
	if err := query.validate(); err != nil {
		return out, err
	}
	operator, err := operatorPrincipal(r.certificate)
	if err != nil {
		return out, err
	}
	adjacent, err := r.reporter()
	if err != nil {
		return out, err
	}
	out.Source = adjacent.Principal()
	source := r.serviceTopology
	if source == nil {
		source = r.topology
	}
	view, err := source(ctx)
	if err != nil {
		return out, err
	}
	now := time.Now()
	if view == nil || !view.RuntimeAvailable || view.SelfPrincipal != adjacent.Principal() || view.SelfTenantId != operator.Tenant().String() || view.SelfDeviceId != adjacent.UUID().String() || view.SelfMeshAddress != adjacent.MeshAddress().String() || view.CapturedAtUnixMs > now.Add(time.Second).UnixMilli() || now.UnixMilli()-view.CapturedAtUnixMs > 10000 {
		return out, nil
	}
	out.Captured = view.CapturedAtUnixMs
	out.Truncated = view.Truncated
	if !view.ServicesIncluded {
		out.State = "unsupported"
		return out, nil
	}
	out.State = "ready"
	seen := make(map[string]bool)
	bytes := 512
outer:
	for _, hint := range view.Announcements {
		device, err := meshid.DeviceFromPrincipal(hint.Principal)
		if err != nil || device.Tenant() != operator.Tenant() || !serviceOriginLive(view, adjacent, device, hint, now) {
			continue
		}
		for _, service := range hint.Services {
			if service.Port == 0 || service.Port > 65535 || service.Type == "" || (query.Mesh != "" && query.Mesh != service.Mesh) || (query.Type != "" && query.Type != service.Type) {
				continue
			}
			key := hint.Principal + "\x00" + service.Mesh + "\x00" + service.AppId + "\x00" + service.ServiceId
			if seen[key] {
				continue
			}
			entry := OperatorServiceInfo{Selector: MeshSelector(device), Principal: device.Principal(), Name: hint.Name, Platform: hint.Platform, Mesh: service.Mesh, App: service.AppId, Service: service.ServiceId, Type: service.Type, Instance: service.Instance, Port: service.Port, TXT: append([]string(nil), service.Txt...), Expires: hint.ExpiresAtUnixMs, Revision: hint.Revision}
			encoded, err := json.Marshal(entry)
			if err != nil {
				continue
			}
			if len(out.Services) == operatorServicesRows || bytes+len(encoded)+1 > operatorServicesBytes {
				out.Truncated = true
				break outer
			}
			seen[key] = true
			bytes += len(encoded) + 1
			out.Services = append(out.Services, entry)
		}
	}
	sort.Slice(out.Services, func(i, j int) bool {
		a, b := out.Services[i], out.Services[j]
		return a.Principal+"\x00"+a.Mesh+"\x00"+a.App+"\x00"+a.Service < b.Principal+"\x00"+b.Mesh+"\x00"+b.App+"\x00"+b.Service
	})
	return out, nil
}

func (r *operatorTargets) localServices(stream grpc.ServerStream, a *activity) (bool, error) {
	method, _ := grpc.MethodFromServerStream(stream)
	if method != operatorServicesMethod {
		return false, nil
	}
	a.activeRPCs.Add(1)
	a.touch()
	defer func() { a.activeRPCs.Add(-1); a.touch() }()
	var request rawMessage
	if err := stream.RecvMsg(&request); err != nil {
		return true, err
	}
	var query OperatorServiceQuery
	if len(request) > 256 || json.Unmarshal(request, &query) != nil {
		return true, status.Error(codes.InvalidArgument, "invalid mesh service query")
	}
	ctx, cancel := context.WithTimeout(stream.Context(), 15*time.Second)
	defer cancel()
	view, err := r.serviceSnapshot(ctx, query)
	if err != nil {
		return true, err
	}
	wire, err := json.Marshal(view)
	if err != nil {
		return true, err
	}
	if len(wire) > operatorServicesBytes {
		return true, status.Error(codes.ResourceExhausted, "mesh service observation exceeds reply budget")
	}
	reply := rawMessage(wire)
	return true, stream.SendMsg(&reply)
}

func ReadOperatorServices(ctx context.Context, query OperatorServiceQuery) (OperatorServices, error) {
	if err := query.validate(); err != nil {
		return OperatorServices{}, err
	}
	owner, err := prepareOperatorDirectory(ctx)
	if err != nil {
		return OperatorServices{Version: 1, State: "not-connected", Services: []OperatorServiceInfo{}}, err
	}
	defer owner.Close()
	wire, err := json.Marshal(query)
	if err != nil {
		return OperatorServices{}, err
	}
	request, reply := rawMessage(wire), rawMessage{}
	if err := owner.Conn.Invoke(ctx, operatorServicesMethod, &request, &reply, grpc.ForceCodec(rawCodec{}), grpc.MaxCallRecvMsgSize(operatorServicesBytes)); err != nil {
		return OperatorServices{}, err
	}
	var result OperatorServices
	if json.Unmarshal(reply, &result) != nil || result.Version != 1 || len(result.Services) > operatorServicesRows {
		return result, errors.New("unsupported operator service observation")
	}
	return result, nil
}
