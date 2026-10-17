package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Tunnel request signatures use the primary operator identity. An explicitly
// supplied signing pair must belong to the active session's operator principal.
func tunnelPrincipalSigner(auth *config.AuthConfig) (func([]byte) ([]byte, error), error) {
	return configuredTunnelSigner(auth, cloudrelay.PrincipalSigner)
}

// liteRelayOptions never puts the ML-DSA primary on the Lite lane: it uses the
// explicit WENDY_LITE_OPERATOR_* override, else the session's Lite leaf,
// requesting one when missing, stale, or renew is set.
func liteRelayOptions(ctx context.Context, auth *config.AuthConfig, renew bool) (liteclient.RelayOptions, error) {
	options := liteclient.RelayOptions{}
	if auth == nil || len(auth.Certificates) == 0 {
		return options, fmt.Errorf("no operator certificate")
	}
	var cert config.CertificateInfo
	var err error
	if os.Getenv("WENDY_LITE_OPERATOR_CERT") != "" || os.Getenv("WENDY_LITE_OPERATOR_KEY") != "" {
		cert, err = cloudrelay.LiteOperatorIdentity(auth.Certificates[0])
	} else {
		cert, err = liteOperatorCertificate(ctx, auth, renew)
	}
	if err != nil {
		return options, err
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(cert.PemCertificate))
	if err != nil || len(leaves) != 1 {
		return options, fmt.Errorf("invalid Lite operator certificate")
	}
	principal, ok := certs.TenantPrincipalFromCert(leaves[0])
	if !ok || principal != auth.Certificates[0].PrincipalURI {
		return options, fmt.Errorf("Lite operator certificate does not belong to the selected operator")
	}
	// The Lite authority is separate from the tenant's device PKI. Keep its
	// client presentation chain separate from the selected session's trust
	// bundle, including when pki-core issues only a Lite leaf.
	options.DeviceTrustChainPEM = auth.Certificates[0].PemCertificateChain
	if options.DeviceTrustChainPEM == "" {
		return options, fmt.Errorf("selected Cloud session has no device CA chain; run 'wendy auth login'")
	}
	// Resolve the separate classical device authority only when the selected
	// board needs it. Existing ML-DSA sessions keep their current trust path.
	pkiEndpoint, tenantID := auth.PKIEndpoint, auth.Certificates[0].TenantUUID()
	options.DeviceTrustFallback = func(ctx context.Context) (string, error) {
		roots, err := liteenroll.DiscoverP256DeviceRoots(ctx, pkiEndpoint, tenantID)
		return string(roots), err
	}
	key, err := cert.PrivateKeyPEM()
	if err != nil {
		return options, err
	}
	options.SignRequest, err = cloudrelay.LitePrincipalSigner(cert.PemCertificate, []byte(key))
	if err != nil {
		return options, err
	}
	// Freeze key material with the selected certificate, so neither the
	// provider's stored identities nor an environment override can replace it.
	cert.PemPrivateKey = key
	options.OperatorIdentity = cert
	return options, nil
}
func configuredTunnelSigner(auth *config.AuthConfig, signer func(string, []byte) (func([]byte) ([]byte, error), error)) (func([]byte) ([]byte, error), error) {
	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("no operator certificate")
	}
	cert := auth.Certificates[0]
	certPath, keyPath := os.Getenv("WENDY_TUNNEL_SIGNING_CERT"), os.Getenv("WENDY_TUNNEL_SIGNING_KEY")
	if certPath == "" && keyPath == "" {
		key, err := cert.PrivateKeyPEM()
		if err != nil {
			return nil, err
		}
		return signer(cert.PemCertificate, []byte(key))
	}
	if certPath == "" || keyPath == "" {
		return nil, fmt.Errorf("set both WENDY_TUNNEL_SIGNING_CERT and WENDY_TUNNEL_SIGNING_KEY")
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, fmt.Errorf("reading tunnel signing certificate: %w", err)
	}
	leafPEM, err := certs.LeafCertificatePEM(string(certPEM))
	if err != nil {
		return nil, err
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(leafPEM))
	if err != nil || len(leaves) != 1 {
		return nil, fmt.Errorf("invalid tunnel signing certificate")
	}
	principal, ok := certs.TenantPrincipalFromCert(leaves[0])
	if !ok || principal != cert.PrincipalURI {
		return nil, fmt.Errorf("tunnel signing certificate does not belong to the logged-in operator")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, fmt.Errorf("reading tunnel signing key: %w", err)
	}
	defer clear(key)
	return signer(leafPEM, key)
}

// liteRenewMargin: a Lite session lasts at most 1800 s and never outlives the
// operator leaf, so a leaf with less left is re-requested first.
const liteRenewMargin = 30 * time.Minute

// oidLiteOperator is id-kp-wendyLiteOperator, the EKU pki-core sets on every
// P-256 operator leaf and on no other certificate.
var oidLiteOperator = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 65441, 1, 9}

// liteOperatorCertificate returns the session's Lite leaf, requesting a new one
// over the ML-DSA session when it is missing, expires within liteRenewMargin,
// or renew is set (Cloud refused the stored one).
func liteOperatorCertificate(ctx context.Context, auth *config.AuthConfig, renew bool) (config.CertificateInfo, error) {
	if cert, ok := sessionLiteCertificate(auth, liteRenewMargin); ok && !renew {
		return cert, nil
	}
	if auth.OAuthIssuer == "" || auth.ServiceAccount != "" {
		return config.CertificateInfo{}, fmt.Errorf("Wendy Lite through Cloud needs a Lite operator certificate, which pki-core issues only to an operator login; run 'wendy auth login'")
	}
	unlock, err := acquireAuthRefreshLock(ctx)
	if err != nil {
		return config.CertificateInfo{}, err
	}
	defer unlock()
	refused := auth.LiteCertificate
	if err := reloadOAuthSession(auth); err != nil {
		return config.CertificateInfo{}, err
	}
	// Another process may have requested one while we waited for the lock.
	if cert, ok := sessionLiteCertificate(auth, liteRenewMargin); ok && (!renew || auth.LiteCertificate != refused) {
		return cert, nil
	}
	err = refreshOIDCCertificate(ctx, auth, true)
	// The refresh token rotated even if issuance failed; keep it.
	if saveErr := persistOAuthSession(auth); err == nil {
		err = saveErr
	}
	if err != nil {
		return config.CertificateInfo{}, fmt.Errorf("requesting Lite operator certificate: %w", err)
	}
	// pki-core clamps the leaf to the primary's notAfter; use it even if short.
	cert, ok := sessionLiteCertificate(auth, 0)
	if !ok {
		return config.CertificateInfo{}, fmt.Errorf("pki-core issued an unusable Lite operator certificate")
	}
	return cert, nil
}

// sessionLiteCertificate returns the stored Lite leaf when it belongs to the
// session's operator and stays valid for at least margin, or already reaches
// the primary's notAfter (pki-core caps it there, so a new one is no longer).
// A leaf-only response is valid: the Lite authority is provisioned on the
// device, while the primary session carries our trust for device certificates.
func sessionLiteCertificate(auth *config.AuthConfig, margin time.Duration) (config.CertificateInfo, bool) {
	if auth.LiteCertificate == "" || auth.LitePrivateKey == "" || len(auth.Certificates) == 0 {
		return config.CertificateInfo{}, false
	}
	leafPEM, chainPEM, leaf, err := splitCertificateChainPEM([]byte(auth.LiteCertificate))
	if err != nil || !time.Now().Before(leaf.NotAfter) {
		return config.CertificateInfo{}, false
	}
	if time.Until(leaf.NotAfter) <= margin {
		_, _, primary, err := splitCertificateChainPEM([]byte(auth.Certificates[0].PemCertificate))
		if err != nil || leaf.NotAfter.Before(primary.NotAfter) {
			return config.CertificateInfo{}, false
		}
	}
	principal, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok || principal != auth.Certificates[0].PrincipalURI {
		return config.CertificateInfo{}, false
	}
	return config.CertificateInfo{
		PemCertificate:      leafPEM,
		PemCertificateChain: chainPEM,
		PemPrivateKey:       auth.LitePrivateKey,
		PrincipalURI:        principal,
	}, true
}

// checkLiteOperatorLeaf refuses anything but pki-core's Lite profile: a P-256
// key and EKU exactly {clientAuth, id-kp-wendyLiteOperator}.
func checkLiteOperatorLeaf(leafPEM string) error {
	leaves, err := certs.ParseCertsFromPEM([]byte(leafPEM))
	if err != nil || len(leaves) != 1 {
		return fmt.Errorf("invalid Lite operator certificate")
	}
	leaf := leaves[0]
	key, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() ||
		!slices.Equal(leaf.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}) ||
		len(leaf.UnknownExtKeyUsage) != 1 || !leaf.UnknownExtKeyUsage[0].Equal(oidLiteOperator) {
		return fmt.Errorf("pki-core did not return a Lite operator certificate (P-256, clientAuth + 1.3.6.1.4.1.65441.1.9)")
	}
	return nil
}
