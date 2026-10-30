package grpcclient

import (
	"context"
	"crypto/tls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"google.golang.org/grpc/credentials"
	"net"
)

// gRPC retains credentials across reconnects. Keep its bounded CA cache, but
// allocate acknowledgement and snapshot state separately for every handshake.
type issuerTransport struct {
	base     *tls.Config
	material *meshalpn.Certificates
}

func newIssuerTransport(base *tls.Config, chain string) (credentials.TransportCredentials, error) {
	known, err := certs.ParseCertsFromPEM([]byte(chain))
	if err != nil {
		return nil, err
	}
	return &issuerTransport{base: base.Clone(), material: meshalpn.NewCertificates(base.Certificates[0], known)}, nil
}
func (c *issuerTransport) ClientHandshake(ctx context.Context, authority string, conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	config := c.base.Clone()
	c.material.ConfigureClient(config, "h2")
	return credentials.NewTLS(config).ClientHandshake(ctx, authority, conn)
}
func (c *issuerTransport) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return credentials.NewTLS(c.base).ServerHandshake(conn)
}
func (c *issuerTransport) Info() credentials.ProtocolInfo { return credentials.NewTLS(c.base).Info() }
func (c *issuerTransport) Clone() credentials.TransportCredentials {
	return &issuerTransport{base: c.base.Clone(), material: c.material}
}
func (c *issuerTransport) OverrideServerName(name string) error { c.base.ServerName = name; return nil }
