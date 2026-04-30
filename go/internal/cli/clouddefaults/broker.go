package clouddefaults

import (
	"net"
	"strings"
)

// BrokerURL returns the effective tunnel broker endpoint. Wendy Cloud exposes
// the broker on the same public :443 endpoint as cloud gRPC; local/non-cloud
// deployments keep the historical dedicated broker port.
func BrokerURL(cloudGRPC, brokerURL, defaultBrokerPort string) string {
	if brokerURL != "" {
		return brokerURL
	}
	if strings.HasSuffix(cloudGRPC, ":443") {
		return cloudGRPC
	}
	host := cloudGRPC
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return net.JoinHostPort(host, defaultBrokerPort)
}
