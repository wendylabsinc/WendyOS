package commands

import (
	"fmt"
	"os"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// A request-signing certificate is separate from the ECDSA TLS certificate:
// Go TLS cannot present the ML-DSA-only credential required for tunnel reqsigs.
// Until login can obtain both profiles, an explicitly supplied signing pair
// must belong to exactly the same operator principal as the active session.
func tunnelPrincipalSigner(auth *config.AuthConfig) (func([]byte) ([]byte, error), error) {
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
		return cloudrelay.PrincipalSigner(cert.PemCertificate, []byte(key))
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
	return cloudrelay.PrincipalSigner(leafPEM, key)
}
