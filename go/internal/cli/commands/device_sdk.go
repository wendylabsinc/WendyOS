package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/devices"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func sdkLANDevice(d models.LANDevice) devices.LANDevice {
	return devices.LANDevice{Port: d.Port, IsMTLS: d.IsMTLS, IPAddress: d.IPAddress, Hostname: d.Hostname, USB: d.USB}
}

// Core sees public expiry and an index into the immutable host snapshot, never
// config.CertificateInfo's private keys or secret-store references.
func sdkCredentials(all []config.CertificateInfo) []devices.Credential {
	credentials := make([]devices.Credential, len(all))
	for i, cert := range all {
		credentials[i] = devices.Credential{Reference: strconv.Itoa(i), OrganizationID: cert.OrganizationID}
		if leaves, _ := certs.ParseCertsFromPEM([]byte(cert.PemCertificate)); len(leaves) > 0 {
			credentials[i].NotAfter = leaves[0].NotAfter
		}
	}
	return credentials
}

func cliCredentials(all []config.CertificateInfo, selected []devices.Credential) []config.CertificateInfo {
	if len(selected) == 0 {
		return all
	}
	var result []config.CertificateInfo
	for _, credential := range selected {
		i, _ := strconv.Atoi(credential.Reference)
		result = append(result, all[i])
	}
	return result
}

type deviceConnector struct {
	credentials []config.CertificateInfo
	pins        certs.PinChecker
}

func (d deviceConnector) ConnectDevice(ctx context.Context, request wendy.DeviceConnectRequest) (wendy.DeviceConnection, error) {
	var conn *grpcclient.AgentConnection
	var err error
	if request.TLS {
		i, parseErr := strconv.Atoi(request.Credential)
		if parseErr != nil || i < 0 || i >= len(d.credentials) {
			return nil, fmt.Errorf("invalid device credential reference")
		}
		var expected *certs.WendyIdentity
		if id := request.Expected; id != nil {
			expected = &certs.WendyIdentity{OrgID: id.OrgID, TenantUUID: id.TenantUUID, EntityType: id.EntityType, EntityID: id.EntityID, Principal: id.Principal}
		}
		conn, err = grpcclient.ConnectWithTLSExpecting(ctx, request.Address, &d.credentials[i], d.pins, expected)
	} else {
		conn, err = plaintextConnectFn(ctx, request.Address)
	}
	if err != nil {
		return nil, err
	}
	return &deviceConnection{AgentConnection: conn}, nil
}

// A test may replace the host connection factory, while Core still runs the
// actual connection policy and probes through its Transport boundary.
var deviceConnectorFn = func(credentials []config.CertificateInfo, pins certs.PinChecker) wendy.DeviceConnector {
	return deviceConnector{credentials: credentials, pins: pins}
}

type deviceConnection struct{ *grpcclient.AgentConnection }

func (c *deviceConnection) Evidence() wendy.DeviceConnectionEvidence {
	org, _ := c.ObservedServerOrg()
	evidence := wendy.DeviceConnectionEvidence{ObservedOrg: org, TCPDialError: c.TCPDialError()}
	if rejection, ok := identityMismatchFn(c.AgentConnection); ok {
		evidence.IdentityMismatch = rejection
	}
	if rejection, ok := pinMismatchFn(c.AgentConnection); ok {
		evidence.PinMismatch = rejection
	}
	return evidence
}

// Only serialization and the CLI's existing connection-local probe cache live
// here. The request and probe deadline are selected by Core.
func (c *deviceConnection) Unary(ctx context.Context, request wendy.RPCRequest) (wendy.RPCResponse, error) {
	if request.Method != agentpb.WendyAgentService_GetAgentVersion_FullMethodName {
		transport, err := wendy.NewGRPCTransport(c.Conn)
		if err != nil {
			return wendy.RPCResponse{}, err
		}
		response, err := transport.Unary(ctx, request)
		var rpc *wendy.RPCStatusError
		if errors.As(err, &rpc) && rpc.Cause != nil {
			err = &deviceRPCError{rpc}
		}
		return response, err
	}
	message := new(agentpb.GetAgentVersionRequest)
	if err := proto.Unmarshal(request.Message, message); err != nil {
		return wendy.RPCResponse{}, err
	}
	for _, entry := range request.Metadata {
		ctx = metadata.AppendToOutgoingContext(ctx, entry.Name, string(entry.Value))
	}
	var headers, trailers metadata.MD
	response, err := c.AgentService.GetAgentVersion(ctx, message, grpc.Header(&headers), grpc.Trailer(&trailers))
	result := wendy.RPCResponse{InitialMetadata: deviceMetadata(headers), TrailingMetadata: deviceMetadata(trailers)}
	if err != nil {
		st := status.Convert(err)
		return result, &deviceRPCError{RPCStatusError: &wendy.RPCStatusError{Code: wendy.RPCStatusCode(st.Code()), Message: st.Message(), Cause: err, InitialMetadata: result.InitialMetadata, TrailingMetadata: result.TrailingMetadata}}
	}
	c.CacheAgentVersion(response)
	result.Message, err = proto.Marshal(response)
	return result, err
}

func deviceMetadata(md metadata.MD) wendy.Metadata {
	var result wendy.Metadata
	for name, values := range md {
		for _, value := range values {
			result = append(result, wendy.MetadataEntry{Name: name, Value: []byte(value)})
		}
	}
	return result
}

func (c *deviceConnection) ServerStream(ctx context.Context, request wendy.RPCRequest) (wendy.ServerStream, error) {
	transport, err := wendy.NewGRPCTransport(c.Conn)
	if err != nil {
		return nil, err
	}
	return transport.ServerStream(ctx, request)
}

// Preserve existing CLI diagnostics while retaining Core status and the native
// grpc-go cause (including status/details) for errors.Is/As and recovery.
type deviceRPCError struct{ *wendy.RPCStatusError }

func (e *deviceRPCError) Error() string { return e.Cause.Error() }
func (e *deviceRPCError) Unwrap() error { return e.RPCStatusError }

func dialAgentLadderWithCerts(ctx context.Context, target dialTarget, allCerts []config.CertificateInfo) (*grpcclient.AgentConnection, error, error) {
	var pins certs.PinChecker
	if len(allCerts) > 0 && len(target.dialCandidates()) > 0 {
		if store := openPinStore(); store != nil {
			pins = &concurrentPinChecker{store: store}
		}
	}
	coreTarget := devices.Target{PinKey: target.PinKey, PinnedKey: target.PinnedKey, Addr: target.Addr, Candidates: target.Candidates}
	if id := target.Expected; id != nil {
		coreTarget.Expected = &wendy.DeviceIdentity{OrgID: id.OrgID, TenantUUID: id.TenantUUID, EntityType: id.EntityType, EntityID: id.EntityID, Principal: id.Principal}
	}
	host, _, _ := net.SplitHostPort(target.Addr)
	preferredOrg, have := preferredCertOrgForHost(host)
	options := devices.DialOptions{PreferredOrg: preferredOrg, HavePreferredOrg: have, RememberOrg: rememberCertOrg}
	if os.Getenv("WENDY_TLS_DEBUG") != "" {
		options.Debugf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format, args...) }
	}
	result := devices.Dial(ctx, deviceConnectorFn(allCerts, pins), coreTarget, sdkCredentials(allCerts), options)
	if result.Connection != nil {
		return result.Connection.(*deviceConnection).AgentConnection, result.LastMTLSError, nil
	}
	var refusal *devices.IdentityRefusalError
	if errors.As(result.Err, &refusal) {
		var identity *certs.IdentityMismatchError
		if errors.As(refusal.Cause, &identity) {
			return nil, result.LastMTLSError, identityRefusal(refusal.Key, identity)
		}
		var pin *devicepin.PinMismatchError
		if errors.As(refusal.Cause, &pin) {
			return nil, result.LastMTLSError, spkiRefusal(refusal.Key, pin)
		}
	}
	switch result.Failure {
	case devices.CertificateRejected:
		return nil, result.LastMTLSError, chooseRejectionError(ctx, result.ObservedOrg, allCerts, result.Err)
	case devices.PinnedEndpointUnavailable:
		attempts := make([]mtlsAttemptError, len(result.Attempts))
		for i, attempt := range result.Attempts {
			attempts[i] = mtlsAttemptError{addr: attempt.Address, err: attempt.Err}
		}
		failure := pinnedHostNoAuthenticatedEndpointError(target.refusalKey(), target.dialCandidates(), attempts, result.AnyCertRejection)
		failure.pinKey = target.PinKey
		return nil, result.LastMTLSError, failure
	default:
		return nil, result.LastMTLSError, result.Err
	}
}
