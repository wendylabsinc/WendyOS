package meshsession

import (
	"errors"
	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"time"
)

// TLS authenticates at handshake, but app streams can outlive a certificate.
// Bound both endpoints, including resumed sessions, to the local and peer
// verified chain lifetimes. Credentials are immutable; replacing trust creates
// a new client/server and closes the old ones.
func limitSessionLifetime(credentials *localmesh.Credentials, conn *quic.Conn) (time.Time, error) {
	now := time.Now()
	local, err := credentials.Verify(credentials.Certificate.Certificate, now)
	if err != nil {
		return time.Time{}, err
	}
	var chain [][]byte
	for _, cert := range conn.ConnectionState().TLS.PeerCertificates {
		chain = append(chain, cert.Raw)
	}
	peer, err := credentials.Verify(chain, now)
	if err != nil {
		return time.Time{}, err
	}
	if local.Org != credentials.Org || local.Asset != credentials.Asset || peer.Org != local.Org || peer.Asset == local.Asset {
		return time.Time{}, errors.New("mesh app session identity changed")
	}
	expires := local.NotAfter
	if peer.NotAfter.Before(expires) {
		expires = peer.NotAfter
	}
	if !expires.After(time.Now()) {
		return time.Time{}, errors.New("mesh app credentials expired")
	}
	go func() {
		timer := time.NewTimer(time.Until(expires))
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = conn.CloseWithError(1, "mesh app credentials expired")
		case <-conn.Context().Done():
		}
	}()
	return expires, nil
}
