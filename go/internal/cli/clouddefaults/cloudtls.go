package clouddefaults

import (
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"strconv"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// CloudInsecureEnv opts the CLI's Wendy Cloud gRPC dial out of TLS. It has the
// same name and semantics as the agent's (cloudInsecureEnv in
// provisioning_service.go): only an explicit true value (strconv.ParseBool)
// selects plaintext, and it exists only to reach a local plaintext dev cloud.
const CloudInsecureEnv = "WENDY_CLOUD_INSECURE"

// insecureWarningOut receives the plaintext warning. A var so tests can
// capture it.
var insecureWarningOut io.Writer = os.Stderr

// CloudTransportCredentials returns the transport for dialing Wendy Cloud gRPC
// at addr. TLS is the default for every address; when cert is non-nil its leaf
// is presented for mTLS. Previously each caller picked TLS only when addr ended
// in ":443" and sent bearer tokens in cleartext to any other port (WDY-3756).
// Plaintext now requires WENDY_CLOUD_INSECURE and always prints a warning.
func CloudTransportCredentials(addr string, cert *config.CertificateInfo) (credentials.TransportCredentials, error) {
	if v, err := strconv.ParseBool(os.Getenv(CloudInsecureEnv)); err == nil && v {
		fmt.Fprintf(insecureWarningOut, "WARNING: %s is set: dialing Wendy Cloud at %s over plaintext gRPC; credentials are sent unencrypted. Use only against a local development cloud.\n", CloudInsecureEnv, addr)
		return insecure.NewCredentials(), nil
	}
	if cert == nil {
		return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}), nil
	}
	keyPEM, err := cert.PrivateKeyPEM()
	if err != nil {
		return nil, fmt.Errorf("loading client key: %w", err)
	}
	tlsCfg, err := certs.LoadTLSConfig(cert.PemCertificate, cert.PemCertificateChain, keyPEM, "")
	if err != nil {
		return nil, fmt.Errorf("loading TLS config: %w", err)
	}
	return credentials.NewTLS(tlsCfg), nil
}
