package clouddefaults

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/wendylabsinc/wendy-go/cloud"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/legacycertproof"
)

// DefaultBrokerPort is the tunnel broker's gRPC port used when brokerURL is
// derived from auth.CloudGRPC rather than passed explicitly (see BrokerURL).
const DefaultBrokerPort = cloud.DefaultBrokerPort

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
// :443 convention). It picks the broker's trust roots and address, never
// whether TLS or hostname verification applies: WebPKI roots there; installing
// the Wendy CA alone instead causes x509 failures (WDY-2434).
func UsesPublicCA(addr string) bool {
	return cloud.UsesPublicCA(addr)
}

// BrokerTLSConfig builds the client mTLS config for dialing the tunnel broker.
// Standard verification applies on every port: the chain must reach a trusted
// root AND the leaf's DNS/IP SAN must match the dialed host (gRPC sets
// ServerName from the dial target). Only the root set differs: :443 uses the
// system WebPKI roots; local/on-prem brokers use the Wendy CA in
// cert.PemCertificateChain, so their certificate must be issued for the host
// the CLI dials (e.g. a localhost or LAN-IP SAN). Trusting the Wendy CA alone
// was never broker identity: before WDY-3756 non-:443 ports skipped the
// hostname check and accepted any Wendy-CA leaf.
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
		caPool := x509.NewCertPool()
		if certs.AppendChainToPool(caPool, cert.PemCertificateChain) == 0 {
			return nil, fmt.Errorf("no valid CA certificates in PemCertificateChain")
		}
		tlsCfg.RootCAs = caPool
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
	privateKeyPEM, err := cert.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("loading broker proof private key: %w", err)
	}
	identityURI := ""
	if cert.UserID != "" {
		identityURI = fmt.Sprintf("urn:wendy:org:%d:user:%s", cert.OrganizationID, cert.UserID)
	} else if cert.AssetID != 0 {
		identityURI = fmt.Sprintf("urn:wendy:org:%d:asset:%d", cert.OrganizationID, cert.AssetID)
	}
	proofSigner, err := legacycertproof.New(identityURI, cert.PemCertificate, privateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("initializing broker certificate proof: %w", err)
	}

	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)),
		grpc.WithPerRPCCredentials(proofSigner),
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
