package services

import (
	"net"
	"os"

	"google.golang.org/grpc"
)

// LegacyBrokerURL is shared by presence and device-originated v1 tunnels.
// Cloud Run serves both API and broker on 443; direct deployments use 50052.
func LegacyBrokerURL(cloudHost string) string {
	if override := os.Getenv("WENDY_BROKER_URL"); override != "" {
		return override
	}
	host, port, err := net.SplitHostPort(cloudHost)
	if err == nil {
		if port == "443" {
			return cloudHost
		}
		return net.JoinHostPort(host, "50052")
	}
	return net.JoinHostPort(cloudHost, "50052")
}

// dialLegacyCloud preserves certificate-bound request proofs on Cloud Run and
// endpoint mTLS on direct brokers. Callers obtain a fresh proof for each RPC.
func dialLegacyCloud(client *TunnelBrokerClient) (*grpc.ClientConn, brokerRequestMetadata, error) {
	opts, proof, err := client.buildDialOpts()
	if err != nil {
		return nil, nil, err
	}
	conn, err := grpc.NewClient(certificateServiceAddr(client.url), opts...)
	return conn, proof, err
}
