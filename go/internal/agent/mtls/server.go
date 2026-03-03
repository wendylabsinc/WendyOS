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
func NewTLSConfig(certPEM, chainPEM, keyPEM string) (*tls.Config, error) {
	// Build the full certificate chain for the server identity.
	fullChain := certPEM
	if chainPEM != "" {
		fullChain = certPEM + "\n" + chainPEM
	}

	cert, err := tls.X509KeyPair([]byte(fullChain), []byte(keyPEM))
	if err != nil {
		return nil, fmt.Errorf("loading X509 key pair: %w", err)
	}

	// Build a CA pool from the chain to verify client certificates.
	caPool := x509.NewCertPool()
	if chainPEM != "" {
		if !caPool.AppendCertsFromPEM([]byte(chainPEM)) {
			return nil, fmt.Errorf("failed to parse chain PEM for CA pool")
		}
	}
	// Also add the leaf cert itself in case it is self-signed or acts as CA.
	caPool.AppendCertsFromPEM([]byte(certPEM))

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
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
