package localmesh

import (
	"crypto/tls"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"time"
)

// lanAuthenticatedPeer derives the entire identity from the enrolled chain.
// Discovery, source addresses and link hellos cannot choose this identity.
func lanAuthenticatedPeer(c *Credentials, state tls.ConnectionState) (meshid.Device, error) {
	if c == nil || c.Verify == nil || len(state.PeerCertificates) == 0 || len(state.PeerCertificates) > 16 {
		return meshid.Device{}, errors.New("LAN peer certificate missing or oversized")
	}
	chain := make([][]byte, len(state.PeerCertificates))
	for i, cert := range state.PeerCertificates {
		chain[i] = cert.Raw
	}
	id, err := c.VerifyTLS(chain, time.Now())
	if err != nil {
		return meshid.Device{}, err
	}
	if id.Device.IsOperator() || id.Device == (meshid.Device{}) || id.Device == c.Device || id.Device.Tenant() != c.Device.Tenant() {
		return meshid.Device{}, errors.New("LAN peer must be another enrolled device in this tenant")
	}
	return id.Device, nil
}

// A UDP listener cannot pin an advertised peer before ClientHello. Require mTLS
// under current Cloud trust, then derive and pin that authenticated principal at
// admission. VerifyConnection revalidates restored chains on resumption, too.
func lanServerTLS(c *Credentials) (*tls.Config, error) {
	if c == nil || c.Verify == nil || len(c.Certificate.Certificate) == 0 || c.Certificate.PrivateKey == nil {
		return nil, errors.New("LAN listener requires enrolled credentials")
	}
	config := &tls.Config{
		Certificates: []tls.Certificate{c.Certificate},
		MinVersion:   tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{LinkALPN},
		VerifyConnection: func(state tls.ConnectionState) error {
			_, err := lanAuthenticatedPeer(c, state)
			return err
		},
	}
	// Unknown-peer listeners have a separate, bounded server ticket domain.
	// Pinned outbound QUIC still uses the existing per-peer link client cache.
	c.ConfigureTLSServer(config)
	c.ServerTicketStore(LinkALPN, "lan-udp").Configure(config)
	return config, nil
}
