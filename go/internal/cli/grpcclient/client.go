// Package grpcclient provides a gRPC client factory for connecting to the Wendy agent.
package grpcclient

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"

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
	AgentService        agentpb.WendyAgentServiceClient
	ContainerService    agentpb.WendyContainerServiceClient
	AudioService        agentpb.WendyAudioServiceClient
	ProvisioningService agentpb.WendyProvisioningServiceClient
	TelemetryService    agentpb.WendyTelemetryServiceClient
}

// Connect creates an insecure gRPC connection to the agent at the given address.
func Connect(ctx context.Context, address string) (*AgentConnection, error) {
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(insecure.NewCredentials()))
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

	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		return nil, fmt.Errorf("connecting to agent at %s with TLS: %w", address, err)
	}

	ac := newAgentConnection(conn)
	ac.Host = hostFromAddress(address)
	return ac, nil
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
