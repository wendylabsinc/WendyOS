package meshsession

import (
	"net"
	"strconv"
)

// WithAgentService adds reachability to the configured mTLS agent listener.
// The inner agent TLS session still authenticates its own caller and enforces
// operator/device permissions. Plaintext administration is never exposed here.
// Other ports retain the running-app ingress authorization unchanged.
func WithAgentService(apps Authorizer, agentTLSPort uint16) Authorizer {
	return agentServiceAuthorizer{apps: apps, port: agentTLSPort}
}

type agentServiceAuthorizer struct {
	apps Authorizer
	port uint16
}

func (a agentServiceAuthorizer) DialAuthorized(port uint16, dial func(string) (net.Conn, error)) (net.Conn, error) {
	if a.port != 0 && port == a.port {
		return dial(net.JoinHostPort("127.0.0.1", strconv.Itoa(int(a.port))))
	}
	if a.apps == nil {
		return nil, ErrDenied
	}
	return a.apps.DialAuthorized(port, dial)
}
