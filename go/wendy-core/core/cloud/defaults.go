package cloud

import (
	"net"
	"strings"
)

// BrokerURL resolves the tunnel broker's address for a cloud session. An
// explicit brokerURL always wins. Otherwise, when cloudGRPC targets a
// public-CA endpoint (see UsesPublicCA — the production :443 convention),
// the broker shares that same host:port; for any other cloudGRPC
// (local/on-prem), the broker listens on a dedicated port
// (defaultBrokerPort) on that same host.
func BrokerURL(cloudGRPC, brokerURL, defaultBrokerPort string) string {
	if brokerURL != "" {
		return brokerURL
	}
	if UsesPublicCA(cloudGRPC) {
		return cloudGRPC
	}
	host := cloudGRPC
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return net.JoinHostPort(host, defaultBrokerPort)
}

// DefaultBrokerPort is the existing local/on-prem broker port.
const DefaultBrokerPort = "50052"

// UsesPublicCA selects the broker root set/address, never whether TLS applies.
func UsesPublicCA(addr string) bool { return strings.HasSuffix(addr, ":443") }
