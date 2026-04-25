// Package mtls provides helpers for creating gRPC servers with mutual TLS authentication.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// NewTLSConfig creates a TLS config from PEM-encoded certificate, chain, and private key.
// The certificate and chain are concatenated to form the full server certificate chain.
// Client certificates are required and verified against the chain as a CA pool.
// ML-DSA (post-quantum) signed certificates are handled via a custom VerifyPeerCertificate
// callback because Go's crypto/x509 does not natively support ML-DSA signature verification.
func NewTLSConfig(certPEM, chainPEM, keyPEM string) (*tls.Config, error) {
	fullChain := certPEM
	if chainPEM != "" {
		fullChain = certPEM + "\n" + chainPEM
	}

	cert, err := tls.X509KeyPair([]byte(fullChain), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("loading X509 key pair: %w", err)
	}

	caPool := x509.NewCertPool()
	var caCerts []*x509.Certificate
	if chainPEM != "" {
		caPool.AppendCertsFromPEM([]byte(chainPEM))
		caCerts, err = parseCertsFromPEM([]byte(chainPEM))
		if err != nil {
			return nil, fmt.Errorf("parsing chain PEM: %w", err)
		}
	}
	caPool.AppendCertsFromPEM([]byte(certPEM))

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// RequireAnyClientCert requires the client to present a cert but defers
		// chain verification to VerifyPeerCertificate, which handles ML-DSA.
		ClientAuth:            tls.RequireAnyClientCert,
		ClientCAs:             caPool,
		MinVersion:            tls.VersionTLS12,
		VerifyPeerCertificate: buildVerifyPeerCertificate(caPool, caCerts),
	}, nil
}

// NewServer creates a gRPC server with mTLS credentials.
// Additional gRPC server options can be passed and will be applied alongside the TLS credentials.
func NewServer(certPEM, chainPEM, keyPEM string, extraOpts ...grpc.ServerOption) (*grpc.Server, error) {
	tlsConfig, err := NewTLSConfig(certPEM, chainPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("creating TLS config: %w", err)
	}

	creds := credentials.NewTLS(tlsConfig)
	opts := []grpc.ServerOption{grpc.Creds(creds)}
	opts = append(opts, extraOpts...)
	return grpc.NewServer(opts...), nil
}
