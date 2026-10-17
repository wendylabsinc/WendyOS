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
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// addRelayDeviceTrustFallback keeps the normal verifier authoritative. A
// separate tenant device CA is discovered only for a matching P-256 peer whose
// issuer is absent from the selected session's existing trust bundle.
func addRelayDeviceTrustFallback(ctx context.Context, cfg *tls.Config, operator tls.Certificate, chain, deviceID string, discover func(context.Context) (string, error)) {
	if discover == nil {
		return
	}
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
		principal := "spiffe://wendy.sh/tenant/" + own.TenantUUID + "/device/lite-" + strings.TrimPrefix(deviceID, "lite-")
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
		roots, discoverErr := discover(ctx)
		if discoverErr != nil {
			return fmt.Errorf("discovering P-256 device trust: %w", discoverErr)
		}
		fallback, buildErr := deviceTLSConfig(operator, roots, deviceID)
		if buildErr != nil {
			return buildErr
		}
		return fallback.VerifyConnection(state)
	}
}
