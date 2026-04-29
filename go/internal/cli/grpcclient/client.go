// Package grpcclient provides a gRPC client factory for connecting to the Wendy agent.
package grpcclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	grpcInitialStreamWindow = 8 * 1024 * 1024
	grpcInitialConnWindow   = 16 * 1024 * 1024
	grpcReadBufferSize      = 256 * 1024
	grpcWriteBufferSize     = 256 * 1024
)

// AgentConnection holds a gRPC connection and typed service clients.
type AgentConnection struct {
	Conn                *grpc.ClientConn
	Host                string // hostname or IP of the connected agent
	IsMTLS              bool   // true when connected via mutual TLS
	AgentService        agentpb.WendyAgentServiceClient
	ContainerService    agentpb.WendyContainerServiceClient
	AudioService        agentpb.WendyAudioServiceClient
	VideoService        agentpb.WendyVideoServiceClient
	ProvisioningService agentpb.WendyProvisioningServiceClient
	TelemetryService    agentpb.WendyTelemetryServiceClient
	FileSyncService     agentpb.WendyFileSyncServiceClient
}

// Connect creates an insecure gRPC connection to the agent at the given address.
func Connect(ctx context.Context, address string) (*AgentConnection, error) {
	conn, err := grpc.NewClient(
		grpcTarget(address),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithInitialWindowSize(grpcInitialStreamWindow),
		grpc.WithInitialConnWindowSize(grpcInitialConnWindow),
		grpc.WithReadBufferSize(grpcReadBufferSize),
		grpc.WithWriteBufferSize(grpcWriteBufferSize),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to agent at %s: %w", address, err)
	}

	ac := newAgentConnection(conn)
	ac.Host = hostFromAddress(address)
	return ac, nil
}

// ConnectWithTLS creates an mTLS connection using certificates from config.
func ConnectWithTLS(ctx context.Context, address string, certInfo *config.CertificateInfo) (*AgentConnection, error) {
	tlsCfg, err := certs.LoadTLSConfig(
		certInfo.PemCertificate,
		certInfo.PemCertificateChain,
		certInfo.PemPrivateKey,
		"", // use system roots
	)
	if err != nil {
		return nil, fmt.Errorf("loading TLS config: %w", err)
	}

	tlsCfg.InsecureSkipVerify = true // agent uses self-signed certs
	tlsCfg.MinVersion = tls.VersionTLS12

	conn, err := grpc.NewClient(
		grpcTarget(address),
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithInitialWindowSize(grpcInitialStreamWindow),
		grpc.WithInitialConnWindowSize(grpcInitialConnWindow),
		grpc.WithReadBufferSize(grpcReadBufferSize),
		grpc.WithWriteBufferSize(grpcWriteBufferSize),
	)
	if err != nil {
		return nil, fmt.Errorf("connecting to agent at %s with TLS: %w", address, err)
	}

	ac := newAgentConnection(conn)
	ac.Host = hostFromAddress(address)
	ac.IsMTLS = true
	return ac, nil
}

// grpcTarget converts a host:port address into a gRPC target string.
// IPv6 link-local addresses contain a zone ID with a bare "%" (e.g.
// [fe80::1%en0]:50051). grpc.NewClient parses the target as a URL, where
// "%" starts a percent-encoding sequence — "%en" is invalid hex and fails.
// We use the passthrough scheme with url.URL which correctly escapes the
// zone "%" to "%25". The passthrough resolver decodes it back to the
// original zone ID before passing it to the dialer.
//
// The address MUST be bracketed for IPv6 (e.g. [fe80::1%en0]:50051).
// As a safety net, if an unbracketed IPv6 address is received, we add
// brackets before constructing the URL so the host is unambiguous.
func grpcTarget(address string) string {
	if !strings.Contains(address, "%") {
		return address
	}

	// Ensure IPv6 address is properly bracketed. net.SplitHostPort
	// handles [host]:port but fails for bare IPv6 like
	// fe80::1%en0:50051 where the colons are ambiguous.
	if _, _, err := net.SplitHostPort(address); err != nil && !strings.HasPrefix(address, "[") {
		// Zone IDs (interface names) never contain colons, so the
		// port follows the last ":".
		if i := strings.LastIndex(address, ":"); i > 0 {
			host, port := address[:i], address[i+1:]
			address = net.JoinHostPort(host, port)
		}
	}

	u := &url.URL{Scheme: "passthrough", Path: "/" + address}
	return u.String()
}

// hostFromAddress extracts the hostname/IP from a host:port address string.
// Handles IPv6 addresses like [::1]:50051.
func hostFromAddress(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	return host
}

// Close closes the underlying gRPC connection.
func (c *AgentConnection) Close() error {
	if c.Conn != nil {
		return c.Conn.Close()
	}
	return nil
}

func newAgentConnection(conn *grpc.ClientConn) *AgentConnection {
	return &AgentConnection{
		Conn:                conn,
		AgentService:        agentpb.NewWendyAgentServiceClient(conn),
		ContainerService:    agentpb.NewWendyContainerServiceClient(conn),
		AudioService:        agentpb.NewWendyAudioServiceClient(conn),
		VideoService:        agentpb.NewWendyVideoServiceClient(conn),
		ProvisioningService: agentpb.NewWendyProvisioningServiceClient(conn),
		TelemetryService:    agentpb.NewWendyTelemetryServiceClient(conn),
		FileSyncService:     agentpb.NewWendyFileSyncServiceClient(conn),
	}
}

// NewFromConn wraps an existing gRPC connection as an AgentConnection.
// Use this when the caller manages its own dialing (e.g. a cloud tunnel).
func NewFromConn(conn *grpc.ClientConn) *AgentConnection {
	return newAgentConnection(conn)
}
