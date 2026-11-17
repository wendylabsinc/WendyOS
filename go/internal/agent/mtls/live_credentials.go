package mtls

import (
	"crypto/x509"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
	"google.golang.org/grpc/credentials"
)

// Retain issuer candidates on the connection, rather than reconstructing them
// for every message or depending on the shared issuer cache for its lifetime.
// Candidates remain untrusted: live checks use the original verifier's roots.
type liveCredentials struct {
	credentials.TransportCredentials
	material *meshalpn.Certificates
}

func (c *liveCredentials) ServerHandshake(conn net.Conn) (net.Conn, credentials.AuthInfo, error) {
	conn, auth, err := c.TransportCredentials.ServerHandshake(conn)
	if err != nil {
		return conn, auth, err
	}
	info, ok := auth.(credentials.TLSInfo)
	if !ok {
		return conn, auth, nil
	}
	raw := make([][]byte, len(info.State.PeerCertificates))
	for i, cert := range info.State.PeerCertificates {
		raw[i] = cert.Raw
	}
	complete := c.material.Candidates(raw, time.Now())
	info.State.PeerCertificates = append([]*x509.Certificate(nil), info.State.PeerCertificates...)
	for _, der := range complete[len(raw):] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		info.State.PeerCertificates = append(info.State.PeerCertificates, cert)
	}
	return conn, info, nil
}

func (c *liveCredentials) Clone() credentials.TransportCredentials {
	return &liveCredentials{TransportCredentials: c.TransportCredentials.Clone(), material: c.material}
}
