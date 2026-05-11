// Package mtls provides helpers for creating gRPC servers with mutual TLS authentication.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// NewTLSConfig creates a TLS config from PEM-encoded certificate, chain, and private key.
// The certificate and chain are concatenated to form the full server certificate chain.
// Client certificates are required and verified against the chain as a CA pool.
// ML-DSA (post-quantum) signed certificates are handled via a custom VerifyPeerCertificate
// callback because Go's crypto/x509 does not natively support ML-DSA signature verification.
func NewTLSConfig(certPEM, chainPEM, keyPEM string) (*tls.Config, error) {
	// Only include the leaf cert in the TLS certificate — not the chain.
	// Go's TLS library calls x509.ParseCertificate on every cert sent in the
	// handshake, and ML-DSA chain certs (from pki-core) cause parse failures
	// on the receiving client. The chain is used below only for the CA pool.
	leafPEM, err := certs.LeafCertificatePEM(certPEM)
	if err != nil {
		return nil, fmt.Errorf("extracting leaf certificate: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(leafPEM), []byte(keyPEM))
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
		if len(caCerts) == 0 {
			return nil, fmt.Errorf("parsing chain PEM: no certificates found")
		}
	}
	caPool.AppendCertsFromPEM([]byte(certPEM))

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		// RequireAnyClientCert requires the client to present a cert but defers
		// chain verification to VerifyPeerCertificate, which handles ML-DSA.
		ClientAuth: tls.RequireAnyClientCert,
		// ClientCAs intentionally nil: AppendCertsFromPEM cannot parse ML-DSA
		// chain certs (trailing data), so the pool would only contain the leaf
		// cert's subject. Go's TLS client only sends its certificate when its
		// issuer appears in the server's AcceptableCAs list; with a mismatched
		// list it sends nothing and the handshake fails with "certificate required".
		// An empty ClientCAs list signals "accept any CA" — VerifyPeerCertificate
		// performs the actual ML-DSA-aware chain verification instead.
		ClientCAs:             nil,
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
	opts := []grpc.ServerOption{
		grpc.Creds(creds),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             5 * time.Second,
			PermitWithoutStream: true,
		}),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    30 * time.Second,
			Timeout: 10 * time.Second,
		}),
	}
	opts = append(opts, extraOpts...)
	return grpc.NewServer(opts...), nil
}
