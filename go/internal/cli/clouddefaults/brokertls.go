package clouddefaults

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// DefaultBrokerPort is the tunnel broker's gRPC port used when brokerURL is
// derived from auth.CloudGRPC rather than passed explicitly (see BrokerURL).
const DefaultBrokerPort = "50052"

const (
	// KeepalivePing is how often the client sends an HTTP/2 keepalive ping
	// over the broker connection. It must stay >= the agent's MinTime
	// enforcement policy (10s) and frequent enough to keep the tunnel/NAT
	// warm.
	KeepalivePing = 30 * time.Second
	// KeepaliveACKTimeout is how long to wait for a keepalive ACK before
	// declaring the connection dead. It is generous because long OS-update
	// streams run while the device is saturated (artifact download + OS
	// install), and a busy device can take well over the usual 10s to ACK a
	// ping; a tighter window tears down the stream mid-install.
	KeepaliveACKTimeout = 20 * time.Second
)

// UsesPublicCA reports whether addr targets a public-CA endpoint (the production
// :443 convention). WebPKI verification applies there; installing the Wendy-CA
// VerifyConnection pin instead causes x509 failures (WDY-2434).
func UsesPublicCA(addr string) bool {
	return strings.HasSuffix(addr, ":443")
}

// BrokerTLSConfig builds the client mTLS config for dialing the tunnel broker.
// Non-:443 (local/on-prem): skip hostname verification, pin the chain to the
// Wendy CA in cert.PemCertificateChain via VerifyConnection. :443: leave
// standard WebPKI verification in place.
func BrokerTLSConfig(cert config.CertificateInfo, brokerURL string) (*tls.Config, error) {
	keyPEM, err := cert.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("loading client key: %w", err)
	}
	tlsCfg, err := certs.LoadTLSConfig(
		cert.PemCertificate,
		cert.PemCertificateChain,
		keyPEM,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("loading broker TLS config: %w", err)
	}

	if !UsesPublicCA(brokerURL) {
		// For non-standard ports (local/on-prem broker) the server presents a cert
		// signed by the Wendy CA, not a public CA. Skip hostname verification but
		// validate the chain against the stored Wendy CA bundle.
		caPool := x509.NewCertPool()
		if certs.AppendChainToPool(caPool, cert.PemCertificateChain) == 0 {
			return nil, fmt.Errorf("no valid CA certificates in PemCertificateChain")
		}
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // Hostname verification is intentionally skipped for non-standard broker endpoints; VerifyConnection validates the chain against the Wendy CA.
		tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("broker presented no TLS certificate")
			}
			intermediates := x509.NewCertPool()
			for _, c := range cs.PeerCertificates[1:] {
				intermediates.AddCert(c)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         caPool,
				Intermediates: intermediates,
				KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			})
			return err
		}
	}

	return tlsCfg, nil
}

// DialBroker dials the tunnel broker over client mTLS built from auth's
// first certificate. brokerURL is resolved via BrokerURL (an explicit value
// wins; otherwise it's derived from auth.CloudGRPC and DefaultBrokerPort).
// The standard dial option set (transport creds, stream/connection windows,
// read/write buffers, keepalive) is applied first; extra is appended after
// it, letting callers layer on additional options (e.g. a context dialer)
// without duplicating the base set. This is the single dial path for both
// the CLI and the MCP server (WDY-2434): previously each had its own copy,
// and the MCP copy pinned the Wendy CA unconditionally, breaking x509
// verification against the production (public-CA, :443) broker. Routing
// through here also means the MCP path picks up the window/buffer/keepalive
// tuning the CLI already had and it lacked.
func DialBroker(auth *config.AuthConfig, brokerURL string, extra ...grpc.DialOption) (*grpc.ClientConn, error) {
	brokerURL = BrokerURL(auth.CloudGRPC, brokerURL, DefaultBrokerPort)

	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]

	tlsCfg, err := BrokerTLSConfig(cert, brokerURL)
	if err != nil {
		return nil, err
	}

	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithInitialWindowSize(8 * 1024 * 1024),
		grpc.WithInitialConnWindowSize(16 * 1024 * 1024),
		grpc.WithReadBufferSize(256 * 1024),
		grpc.WithWriteBufferSize(256 * 1024),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                KeepalivePing,
			Timeout:             KeepaliveACKTimeout,
			PermitWithoutStream: true,
		}),
	}, extra...)

	conn, err := grpc.NewClient(brokerURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to broker at %s: %w", brokerURL, err)
	}
	return conn, nil
}
