package operatormesh

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/estcacerts"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

const maxStoredLiteIssuers = 128 << 10

// LiteIssuers refreshes the operator's public chain-building material and
// falls back to the last valid path after an offline broker restart.
func LiteIssuers(ctx context.Context, directory, endpoint string, certificate config.CertificateInfo) (string, error) {
	return loadLiteIssuers(ctx, directory, endpoint, certificate, liteenroll.DiscoverLiteDeviceCAChain)
}

func loadLiteIssuers(ctx context.Context, directory, endpoint string, certificate config.CertificateInfo,
	fetch func(context.Context, string, string) (string, error)) (string, error) {
	operator, err := meshoperator.ParsePrincipal(certificate.CertificatePrincipal())
	if err != nil {
		return "", err
	}
	key := sha256.Sum256([]byte(operator.Principal() + "\x00" + endpoint))
	path := filepath.Join(directory, fmt.Sprintf("operator-lite-issuers-%x.pem", key))
	now := time.Now()
	validate := func(pem string) (string, error) {
		bundle, err := certs.ParseCertsFromPEM([]byte(pem))
		if err != nil {
			return "", err
		}
		issuers, _, err := estcacerts.LitePathForEnrolledPeer(bundle, certificate.PemCertificateChain, operator.Tenant().String(), now)
		if err != nil {
			return "", err
		}
		return estcacerts.PEM(issuers), nil
	}
	bundle, err := fetch(ctx, endpoint, operator.Tenant().String())
	if err == nil {
		bundle, err = validate(bundle)
		if err == nil {
			// The live path still works if the optional cache cannot be saved.
			_ = atomicfile.Write(path, []byte(bundle), 0600)
			return bundle, nil
		}
	}
	if file, openErr := os.Open(path); openErr == nil {
		defer file.Close()
		if cached, readErr := io.ReadAll(io.LimitReader(file, maxStoredLiteIssuers+1)); readErr == nil && len(cached) <= maxStoredLiteIssuers {
			if valid, verifyErr := validate(string(cached)); verifyErr == nil {
				return valid, nil
			}
		}
	}
	return "", err
}
