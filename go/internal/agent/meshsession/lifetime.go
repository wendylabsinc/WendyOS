package meshsession

import (
	"crypto/tls"
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
	expires, err := verifiedSessionExpiry(credentials, conn.ConnectionState().TLS)
	if err != nil {
		return time.Time{}, err
	}
	go func() {
		timer := time.NewTimer(time.Until(expires))
		defer timer.Stop()
		select {
		case <-timer.C:
			_ = conn.CloseWithError(1, "net app credentials expired")
		case <-conn.Context().Done():
		}
	}()
	return expires, nil
}

func verifiedSessionExpiry(credentials *localmesh.Credentials, state tls.ConnectionState) (time.Time, error) {
	now := time.Now()
	local, err := credentials.Verify(credentials.Certificate.Certificate, now)
	if err != nil {
		return time.Time{}, err
	}
	var chain [][]byte
	for _, cert := range state.PeerCertificates {
		chain = append(chain, cert.Raw)
	}
	peer, err := credentials.Verify(chain, now)
	if err != nil {
		return time.Time{}, err
	}
	if local.Device != credentials.Device || peer.Device.Tenant() != local.Device.Tenant() || peer.Device == local.Device {
		return time.Time{}, errors.New("net app session identity changed")
	}
	expires := local.NotAfter
	if peer.NotAfter.Before(expires) {
		expires = peer.NotAfter
	}
	if !expires.After(time.Now()) {
		return time.Time{}, errors.New("net app credentials expired")
	}
	return expires, nil
}
