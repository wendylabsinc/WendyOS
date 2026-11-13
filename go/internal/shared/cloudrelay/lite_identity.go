package cloudrelay

import (
	"encoding/pem"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"os"
)

// LiteOperatorIdentity loads the explicitly provisioned P-256 identity for the
// same operator. The active ML-DSA credential still signs Cloud administration RPCs.
// The certificate file contains the leaf followed by its compatible CA chain.
func LiteOperatorIdentity(active config.CertificateInfo) (config.CertificateInfo, error) {
	certPath, keyPath := os.Getenv("WENDY_LITE_OPERATOR_CERT"), os.Getenv("WENDY_LITE_OPERATOR_KEY")
	if certPath == "" && keyPath == "" {
		return active, nil
	}
	if certPath == "" || keyPath == "" {
		return config.CertificateInfo{}, fmt.Errorf("set both WENDY_LITE_OPERATOR_CERT and WENDY_LITE_OPERATOR_KEY")
	}
	certificate, err := os.ReadFile(certPath)
	if err != nil {
		return config.CertificateInfo{}, err
	}
	block, chain := pem.Decode(certificate)
	if block == nil || block.Type != "CERTIFICATE" || len(chain) == 0 {
		return config.CertificateInfo{}, fmt.Errorf("Lite operator certificate must include its CA chain")
	}
	leafPEM := string(pem.EncodeToMemory(block))
	leaves, err := certs.ParseCertsFromPEM([]byte(leafPEM))
	if err != nil || len(leaves) != 1 {
		return config.CertificateInfo{}, fmt.Errorf("invalid Lite operator certificate")
	}
	principal, ok := certs.TenantPrincipalFromCert(leaves[0])
	if !ok || principal != active.PrincipalURI {
		return config.CertificateInfo{}, fmt.Errorf("Lite operator certificate does not belong to the selected operator")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		return config.CertificateInfo{}, err
	}
	defer clear(key)
	if _, err := LitePrincipalSigner(leafPEM, key); err != nil {
		return config.CertificateInfo{}, err
	}
	result := active
	result.PemCertificate = leafPEM
	result.PemCertificateChain = string(chain)
	result.PemPrivateKey = string(key)
	return result, nil
}
