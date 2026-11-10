package meshsession

import (
	"net"
	"net/netip"
	"strconv"
)

// WithAgentService adds reachability to the configured mTLS agent listener.
// The inner agent TLS session still authenticates its own caller and enforces
// operator/device permissions. Plaintext administration is never exposed here.
// Other ports retain the running-app ingress authorization unchanged.
func WithAgentService(apps Authorizer, agentTLSPort uint16) Authorizer {
	return agentServiceAuthorizer{apps: apps, port: agentTLSPort}
}

// WithAgentServiceDial connects the native agent port to a local, mesh-only
// TLS ingress. Server invokes this authorizer only after authenticating the
// outer app session. The returned connection carries ordinary inner TLS and
// does not inherit the outer peer's identity or permissions. Other application
// ports continue using their normal registry and network dialer.
func WithAgentServiceDial(apps Authorizer, agentTLSPort uint16, dial func() (net.Conn, error)) Authorizer {
	if dial == nil {
		dial = func() (net.Conn, error) { return nil, ErrDenied }
	}
	return agentServiceAuthorizer{apps: apps, port: agentTLSPort, agentDial: dial}
}

type agentServiceAuthorizer struct {
	apps      Authorizer
	port      uint16
	agentDial func() (net.Conn, error)
}

func (a agentServiceAuthorizer) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if a.port != 0 && port == a.port {
		if a.agentDial != nil {
			return a.agentDial()
		}
		return dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(int(a.port))))
	}
	if a.apps == nil {
		return nil, ErrDenied
	}
	return a.apps.DialAuthorized(port, dial)
}

// Preserve application datagram ownership when the TCP-only native agent
// exception wraps an app registry. The agent port itself gains no UDP grant.
func (a agentServiceAuthorizer) UDPToken(port uint16) uint64 {
	if auth, ok := a.apps.(UDPAuthorizer); ok {
		return auth.UDPToken(port)
	}
	return 0
}
func (a agentServiceAuthorizer) UDPDone(port uint16, token uint64) <-chan struct{} {
	if auth, ok := a.apps.(UDPAuthorizer); ok {
		return auth.UDPDone(port, token)
	}
	done := make(chan struct{})
	close(done)
	return done
}
func (a agentServiceAuthorizer) WithAuthorizedUDPToken(port uint16, token uint64, send func(netip.AddrPort) error) error {
	if auth, ok := a.apps.(UDPAuthorizer); ok {
		return auth.WithAuthorizedUDPToken(port, token, send)
	}
	return ErrDenied
}
