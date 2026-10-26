package liteclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// LiteDeviceCAChain loads the selected tenant's Lite Device CA chain over
// authenticated HTTPS. The chain is public and untrusted: it is used only as
// intermediates that must chain to the session's trust bundle.
type LiteDeviceCAChain func(context.Context) (string, error)

// addLiteDeviceTrust keeps the normal verifier authoritative. Only for a
// matching P-256 peer whose issuer is absent from the selected session's trust
// bundle does it wait for the tenant's Lite Device CA chain, which the caller
// started loading before dialing. The leaf must chain through
// it to the session's own trust bundle; nothing loaded becomes a trust anchor.
func addLiteDeviceTrust(cfg *tls.Config, operator tls.Certificate, chain, deviceID string, lite func() (string, error)) {
	verify := cfg.VerifyConnection
	cfg.VerifyConnection = func(state tls.ConnectionState) error {
		err := verify(state)
		var unknown x509.UnknownAuthorityError
		if err == nil || !errors.As(err, &unknown) || len(state.PeerCertificates) == 0 {
			return err
		}
		leaf := state.PeerCertificates[0]
		key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
		if !ok || key.Curve != elliptic.P256() || leaf.SignatureAlgorithm != x509.ECDSAWithSHA256 {
			return err
		}
		client, parseErr := x509.ParseCertificate(operator.Certificate[0])
		if parseErr != nil {
			return err
		}
		own, ownOK, ownErr := certs.IdentityFromCert(client)
		peer, peerOK, peerErr := certs.IdentityFromCert(leaf)
		principal := "spiffe://wendy.sh/tenant/" + own.TenantUUID + "/device/" + deviceID
		if ownErr != nil || !ownOK || peerErr != nil || !peerOK || peer.Principal != principal {
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
