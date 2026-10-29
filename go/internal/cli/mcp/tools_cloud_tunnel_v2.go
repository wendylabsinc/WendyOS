package mcp

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

// mcpCloudDevice is a cloud device resolved from either the v1 (int32 asset id)
// or v2 (UUID) asset surface, with the relay dispatch behind openTunnel: legacy
// sessions ride the v1 TunnelBrokerService by int32 id; v2/tenant-UUID sessions
// ride the live v2 relay (cloudrelay.OpenTCP) by asset UUID + service name.
// Mirrors commands.cloudDiscoveryDevice, reimplemented because mcp cannot
// import cli/commands.
type mcpCloudDevice struct {
	name     string
	legacyID int32  // valid only when isV2 is false
	key      string // int32-as-string (v1) or asset UUID (v2)
	isV2     bool
}

func (d mcpCloudDevice) GetName() string { return d.name }

// isV2Session reports whether this session resolves and tunnels on v2. A v2
// session carries a SPIFFE operator identity (tenant UUID); a legacy session
// has only an int32 OrganizationID. Same predicate as handleCloudDiscover.
func isV2Session(auth *config.AuthConfig) bool {
	return len(auth.Certificates) > 0 && auth.Certificates[0].TenantUUID() != ""
}

func (s *mcpServer) resolveCloudDevice(ctx context.Context, auth *config.AuthConfig, deviceName string) (mcpCloudDevice, error) {
	if isV2Session(auth) {
		a, err := s.pickCloudAssetV2(ctx, auth, deviceName)
		if err != nil {
			return mcpCloudDevice{}, err
		}
		return mcpCloudDevice{name: a.GetName(), key: a.GetId(), isV2: true}, nil
	}
	a, err := s.pickCloudAsset(ctx, auth, deviceName)
	if err != nil {
		return mcpCloudDevice{}, err
	}
	return mcpCloudDevice{name: a.GetName(), legacyID: a.GetId(), key: fmt.Sprint(a.GetId())}, nil
}

// openTunnel dials the device's remotePort. Legacy sessions use the v1 broker
// (brokerConn) by int32 id; v2 sessions ignore brokerConn and go through the v2
// relay, which selects the broker itself, routing by asset UUID + service name.
func (d mcpCloudDevice) openTunnel(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, remotePort uint32) (net.Conn, error) {
	if !d.isV2 {
		return mcpOpenBrokerTunnel(ctx, brokerConn, auth, d.legacyID, remotePort)
	}
	service, err := mcpTunnelService(remotePort)
	if err != nil {
		return nil, err
	}
	signer, err := mcpTunnelPrincipalSigner(auth)
	if err != nil {
		return nil, err
	}
	cloudCtx, err := mcpCloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	conn, err := mcpDialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	issuer, err := cloudrelay.Issuer(auth.CloudGRPC, os.Getenv("WENDY_CLOUD_GRANT_ISSUER"))
	if err != nil {
		return nil, err
	}
	return cloudrelay.OpenTCP(ctx, cloudCtx, conn, &cloudrelay.Verifier{Issuer: issuer}, d.key, service, signer)
}

// mcpAgentRPCPingSession measures a real request/response over the authorized
// wendy-agent tunnel. Ping RTT is one GetAgentVersion round-trip over
// the tunnelled agent conn. Satisfies mcpPingSession. Mirrors
// commands.agentRPCPingSession.
type mcpAgentRPCPingSession struct {
	ctx     context.Context
	conn    *grpcclient.AgentConnection
	replies chan *cloudpb.TunnelData
}

func newMCPAgentRPCPingSession(ctx context.Context, conn *grpcclient.AgentConnection) *mcpAgentRPCPingSession {
	return &mcpAgentRPCPingSession{ctx: ctx, conn: conn, replies: make(chan *cloudpb.TunnelData, 1)}
}

func (s *mcpAgentRPCPingSession) sendEcho(req *cloudpb.IcmpEchoRequest) error {
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if _, err := s.conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		return err
	}
	reply := &cloudpb.TunnelData{IcmpReply: &cloudpb.IcmpEchoReply{Identifier: req.Identifier, Sequence: req.Sequence, Payload: req.Payload, OriginateUnixNs: req.OriginateUnixNs}}
	select {
	case s.replies <- reply:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *mcpAgentRPCPingSession) recv() (*cloudpb.TunnelData, error) {
	select {
	case reply := <-s.replies:
		return reply, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

// mcpTunnelService shares the CLI's loopback port vocabulary.
func mcpTunnelService(remotePort uint32) (string, error) {
	return cloudrelay.PortService("tcp", remotePort)
}

// mcpTunnelPrincipalSigner builds the ML-DSA operator request signer the v2
// relay authorization needs, from the session's operator certificate.
func mcpTunnelPrincipalSigner(auth *config.AuthConfig) (func([]byte) ([]byte, error), error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("no operator certificate")
	}
	cert := auth.Certificates[0]
	key, err := cert.PrivateKeyPEM()
	if err != nil {
		return nil, err
	}
	return cloudrelay.PrincipalSigner(cert.PemCertificate, []byte(key))
}

// pickCloudAssetV2 mirrors pickCloudAsset over the v2 asset surface: name match
// (case-insensitive; an ambiguous name is an error), then exact UUID-id
// fallback, then an offline-inclusive re-query to tell "enrolled but offline"
// from "never enrolled".
func (s *mcpServer) pickCloudAssetV2(ctx context.Context, auth *config.AuthConfig, deviceName string) (*cloudpbv2.Asset, error) {
	assets, err := mcpListCloudAssetsV2(ctx, auth, "", true)
	if err != nil {
		return nil, err
	}
	if deviceName == "" {
		switch len(assets) {
		case 0:
			return nil, &cloudResolveErr{code: errCodeNotFound, msg: "no enrolled devices found for this org; enroll a device with cloud_enroll_device"}
		case 1:
			return assets[0], nil
		default:
			return nil, &cloudResolveErr{code: errCodeInvalidArgument, msg: "multiple cloud devices found; pass device_name"}
		}
	}

	lower := strings.ToLower(deviceName)
	var matched *cloudpbv2.Asset
	for _, a := range assets {
		if strings.ToLower(a.GetName()) == lower {
			if matched != nil {
				return nil, &cloudResolveErr{code: errCodeInvalidArgument, msg: fmt.Sprintf("multiple devices match %q; use a more specific name", deviceName)}
			}
			matched = a
		}
	}
	if matched == nil {
		// UUID device-id fallback, mirroring the v1 numeric-id fallback. Ids are
		// unique, so no ambiguity check is needed.
		for _, a := range assets {
			if a.GetId() == deviceName {
				matched = a
				break
			}
		}
	}
	if matched != nil {
		return matched, nil
	}

	// No online match: re-check offline-inclusive before concluding it doesn't
	// exist. A re-query failure preserves the NOT_FOUND below rather than
	// mislabelling an API outage as DEVICE_UNREACHABLE (see offlineDeviceErr).
	if all, err := mcpListCloudAssetsV2(ctx, auth, "", false); err == nil {
		for _, a := range all {
			if strings.ToLower(a.GetName()) == lower || a.GetId() == deviceName {
				return nil, &cloudResolveErr{code: errCodeDeviceUnreachable, msg: fmt.Sprintf("device %q is enrolled but currently reported offline; check the device's power and network connection, or call cloud_discover with online_only=false to list enrolled devices", deviceName)}
			}
		}
	}
	return nil, &cloudResolveErr{code: errCodeNotFound, msg: fmt.Sprintf("no device named %q found; call cloud_discover to list devices", deviceName)}
}
