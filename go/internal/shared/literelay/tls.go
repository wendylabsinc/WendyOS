package literelay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshalpn"
)

func DeviceTLSConfig(ctx context.Context, cert tls.Certificate, chain, deviceID string, lite LiteDeviceCAChain) (*tls.Config, error) {
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("missing client certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	opts := certs.ServerVerifyOpts{ChainPEM: chain}
	identity, hasIdentity, err := certs.IdentityFromCert(leaf)
	if err != nil {
		return nil, err
	}
	if !hasIdentity || identity.TenantUUID == "" || !strings.HasPrefix(identity.Principal, "spiffe://wendy.sh/tenant/"+identity.TenantUUID+"/operator/") {
		return nil, fmt.Errorf("Wendy Lite PKI requires a tenant-scoped operator identity")
	}
	if deviceID == "" {
		return nil, fmt.Errorf("missing Wendy Lite device identity")
	}
	// A Lite board's one id is a canonical device UUID. Any other pin, such as
	// a pre-UUID lite- identity, is refused rather than used verbatim.
	if id, err := uuid.Parse(deviceID); err != nil || id.String() != deviceID {
		return nil, fmt.Errorf("Wendy Lite device identity %q is not a device UUID; update the board's firmware, then unenroll and re-enroll it over USB", deviceID)
	}
	opts.ExpectedIdentity = &certs.WendyIdentity{
		EntityType: certs.EntityAsset, EntityID: deviceID, TenantUUID: identity.TenantUUID,
		Principal: "spiffe://wendy.sh/tenant/" + identity.TenantUUID + "/device/" + deviceID,
	}

	verify, err := certs.BuildServerVerifyConnection(opts)
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		InsecureSkipVerify: true, // Verification and device identity checks are performed below.
		VerifyConnection:   verify,
	}
	if lite != nil {
		type loaded struct {
			chain string
			err   error
		}
		result := make(chan loaded, 1)
		go func() {
			chainPEM, err := lite(ctx)
			result <- loaded{chainPEM, err}
		}()
		addLiteDeviceTrust(cfg, cert, chain, deviceID, sync.OnceValues(func() (string, error) {
			r := <-result
			return r.chain, r.err
		}))
	}
	known, err := certs.ParseCertsFromPEM([]byte(chain))
	if err != nil {
		return nil, err
	}
	meshalpn.NewCertificates(cert, known).ConfigureClient(cfg, meshalpn.WendyCom)
	return cfg, nil
}
