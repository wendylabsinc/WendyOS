package literelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// LiteDeviceCAChain loads the selected tenant's Lite Device CA chain over
// authenticated HTTPS. The chain is public and untrusted: it is used only as
// intermediates that must chain to the session's trust bundle.
type LiteDeviceCAChain func(context.Context) (string, error)

// LoadLiteDeviceCAChain starts loading lite under ctx now, typically before
// dialing, and returns a function that waits for the one result.
func LoadLiteDeviceCAChain(ctx context.Context, lite LiteDeviceCAChain) func() (string, error) {
	type loaded struct {
		chain string
		err   error
	}
	result := make(chan loaded, 1)
	go func() {
		chainPEM, err := lite(ctx)
		result <- loaded{chainPEM, err}
	}()
	return sync.OnceValues(func() (string, error) {
		r := <-result
		return r.chain, r.err
	})
}

// AddLiteDeviceTrust keeps the normal verifier authoritative. Only for a P-256
// peer named principal, in own's tenant, whose issuer is absent from chain does
// it wait for the tenant's Lite Device CA chain. The leaf must chain through
// that chain to chain's own certificates; nothing loaded becomes a trust anchor.
func AddLiteDeviceTrust(cfg *tls.Config, own tls.Certificate, chain, principal string, lite func() (string, error)) {
	verify := cfg.VerifyConnection
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		err := verify(state)
		var unknown x509.UnknownAuthorityError
		if err == nil || !errors.As(err, &unknown) || len(state.PeerCertificates) == 0 || len(own.Certificate) == 0 {
			return err
		}
		leaf := state.PeerCertificates[0]
		key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
		if !ok || key.Curve != elliptic.P256() || leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
			return err
		}
		client, parseErr := x509.ParseCertificate(own.Certificate[0])
		if parseErr != nil {
			return err
		}
		self, selfOK, selfErr := certs.IdentityFromCert(client)
		peer, peerOK, peerErr := certs.IdentityFromCert(leaf)
		if selfErr != nil || !selfOK || self.TenantUUID == "" || peerErr != nil || !peerOK ||
			peer.Principal != principal || peer.TenantUUID != self.TenantUUID {
			return err
		}
		// UnknownAuthorityError can also wrap a bad signature under a known
		// issuer. Do not use that error to change the selected trust authority.
		known, parseErr := certs.ParseCertsFromPEM([]byte(chain))
		if parseErr != nil {
			return err
		}
		for _, issuer := range known {
			if bytes.Equal(issuer.RawSubject, leaf.RawIssuer) {
				return err
			}
		}
		litePEM, liteErr := lite()
		if liteErr != nil {
			return fmt.Errorf("discovering Lite device CA chain: %w", liteErr)
		}
		intermediates, parseErr := certs.ParseCertsFromPEM([]byte(litePEM))
		if parseErr != nil || len(intermediates) == 0 {
			return fmt.Errorf("invalid Lite device CA chain")
		}
		intermediates = append(intermediates, state.PeerCertificates[1:]...)
		now := time.Now()
		if verifyErr := certs.VerifyPeerCertificateChain(leaf, intermediates, known, x509.ExtKeyUsageServerAuth, now, now); verifyErr != nil {
			return fmt.Errorf("Lite device certificate does not chain to the session trust: %w", verifyErr)
		}
		return nil
	}
}
