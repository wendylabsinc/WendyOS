package services

import "google.golang.org/grpc"

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
