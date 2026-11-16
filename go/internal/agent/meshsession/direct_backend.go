package meshsession

import (
	"context"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// DirectBackend uses the same live application ingress ownership as upper-layer
// sessions. The caller is already authenticated by the carrier/session manager.
// Native services need their own identity-aware adapters; they are not converted
// into arbitrary port dials. An authorized native mTLS port exposed through the
// existing Authorizer still carries its ordinary inner TLS authentication.
func (s *Server) DirectBackend(ctx context.Context, peer meshid.Device, r directstream.OpenRequest) (net.Conn, error) {
	if !peer.IsValid() || peer.Tenant() != s.credentials.Device.Tenant() || r.Port == 0 {
		return nil, ErrDenied
	}
	if r.Service == directstream.ServiceUDP {
		return s.directUDPBackend(ctx, peer, r.Port, r.DatagramSize)
	}
	if r.Service != directstream.ServiceTCP || r.DatagramSize != 0 {
		return nil, ErrDenied
	}
	setup, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return s.authorizer.DialAuthorized(r.Port, func(address string) (net.Conn, error) {
		var dial net.Dialer
		return dial.DialContext(setup, "tcp4", address)
	})
}
