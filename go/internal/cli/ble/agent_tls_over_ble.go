package ble

import (
	"crypto/tls"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
)

// NewClientTLSConfig builds the mTLS config for talking to the WendyOS agent
// over an L2CAP channel — the CLI's side of the trust decision in ConnectAgent.
// It is Wendy-specific (it depends on the Wendy PKI in shared/certs), which is
// why it lives here rather than in the generic ble/central transport.
//
// The Wendy Lite path does not use this: liteclient builds its own config, with
// its own root pool, in ConnectViaBLEWithMutualAuthentication.
//
// InsecureSkipVerify bypasses Go's built-in verifier (ML-DSA chain certs
// fail to parse; no TLS hostname over L2CAP); opts.PinStore and chain
// verification are handled by the VerifyConnection callback.
func NewClientTLSConfig(certPEM, keyPEM string, opts certs.ServerVerifyOpts) (*tls.Config, error) {
	cert, err := certs.TLSKeyPair(certPEM, opts.ChainPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading BLE client certificate: %w", err)
	}
	verifyConn, err := certs.BuildServerVerifyConnection(opts)
	if err != nil {
		return nil, fmt.Errorf("building BLE server certificate verifier: %w", err)
	}
	cfg := &tls.Config{
		Certificates:       []tls.Certificate{cert},
		InsecureSkipVerify: true, //nolint:gosec — hostname bypass only; VerifyConnection validates server cert against Wendy PKI
		VerifyConnection:   verifyConn,
		MinVersion:         tls.VersionTLS12,
	}
	known, err := certs.ParseCertsFromPEM([]byte(opts.ChainPEM))
	if err != nil {
		return nil, err
	}
	meshalpn.NewCertificates(cert, known).ConfigureClient(cfg, "h2")
	return cfg, nil
}
