// Package grpcclient provides a gRPC client factory for connecting to the Wendy agent.
package grpcclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// AgentConnection holds a gRPC connection and typed service clients.
type AgentConnection struct {
	Conn                *grpc.ClientConn
	Host                string // hostname or IP of the connected agent
	Hostname            string // mDNS .local hostname, if known (preferred for registry operations)
	IsMTLS              bool   // true when connected via mutual TLS
	AgentService        agentpb.WendyAgentServiceClient
	ContainerService    agentpb.WendyContainerServiceClient
	AudioService        agentpb.WendyAudioServiceClient
	ProvisioningService agentpb.WendyProvisioningServiceClient
	TelemetryService    agentpb.WendyTelemetryServiceClient
}

// RegistryHost returns the host to use for the device's container registry.
// It prefers the .local mDNS hostname (which avoids IPv6 formatting issues)
// and falls back to the raw IP/host used for the gRPC connection.
func (c *AgentConnection) RegistryHost() string {
	if c.Hostname != "" {
		return c.Hostname
	}
	return c.Host
}

// Connect creates an insecure gRPC connection to the agent at the given address.
func Connect(ctx context.Context, address string) (*AgentConnection, error) {
	conn, err := grpc.NewClient(grpcTarget(address), grpc.WithTransportCredentials(insecure.NewCredentials()))
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

	conn, err := grpc.NewClient(grpcTarget(address), grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
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
// [fe80::1%en0]:50051) which grpc.NewClient interprets as an invalid
// URL percent-encoding. Using the passthrough scheme avoids URI parsing
// entirely and passes the address straight to the dialer.
func grpcTarget(address string) string {
	if strings.Contains(address, "%") {
		return "passthrough:///" + address
	}
	return address
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
		ProvisioningService: agentpb.NewWendyProvisioningServiceClient(conn),
		TelemetryService:    agentpb.NewWendyTelemetryServiceClient(conn),
	}
}
