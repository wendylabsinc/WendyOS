package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"net"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"google.golang.org/grpc/credentials"
)

// peerCredentials retains the exact completed certificate presentation used by
// the handshake. ConnectionState otherwise contains only the certificates sent
// on the wire, which may omit issuers negotiated through meshalpn. Later RPCs
// must not depend on those issuers remaining in a shared, evictable cache.
type peerCredentials struct {
	credentials.TransportCredentials
	config   *tls.Config
	material *meshalpn.Certificates
}

func newPeerCredentials(config *tls.Config, known []*x509.Certificate) credentials.TransportCredentials {
	return &peerCredentials{TransportCredentials: credentials.NewTLS(config), config: config,
		material: meshalpn.NewCertificates(config.Certificates[0], known)}
}

func (c *peerCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	config := c.config.Clone()
	var authenticated []*x509.Certificate
	previous := config.GetConfigForClient
	config.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		selected := config
		if previous != nil {
			var err error
			selected, err = previous(hello)
			if err != nil {
				return nil, err
			}
			if selected == nil {
				selected = config
			}
		}
		selected = selected.Clone()
		verify := selected.VerifyConnection
		selected.VerifyConnection = func(state tls.ConnectionState) error {
			if verify != nil {
				if err := verify(state); err != nil {
					return err
				}
			}
			authenticated = append([]*x509.Certificate(nil), state.PeerCertificates...)
			return nil
		}
		return selected, nil
	}
	// This wrapper runs after meshalpn supplies the handshake's frozen issuer
	// snapshot, including when a ticket resumes an abbreviated presentation.
	c.material.ConfigureServer(config)
	conn, auth, err := credentials.NewTLS(config).ServerHandshake(raw)
	if err != nil {
		return nil, nil, err
	}
	info := auth.(credentials.TLSInfo)
	info.State.PeerCertificates = authenticated
	return conn, info, nil
}

func (c *peerCredentials) Clone() credentials.TransportCredentials {
	return &peerCredentials{TransportCredentials: c.TransportCredentials.Clone(), config: c.config.Clone(), material: c.material}
}

func (c *peerCredentials) OverrideServerName(name string) error {
	c.config.ServerName = name
	return c.TransportCredentials.OverrideServerName(name)
}
