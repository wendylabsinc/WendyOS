package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatorcert"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func init() { grpcclient.RequestOperatorCertificate = requestOperatorCertificate }

func requestOperatorCertificate(ctx context.Context, login *config.CertificateInfo, target certs.WendyIdentity) (*config.CertificateInfo, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	// Bind the Cloud session to the exact login leaf used for this connection.
	// Never substitute the currently selected organization's credentials.
	for _, entry := range cfg.Auth {
		for _, cert := range entry.Certificates {
			if cert.PemCertificate != login.PemCertificate {
				continue
			}
			auth := entry
			auth.Certificates = []config.CertificateInfo{cert}
			conn, err := dialCloudGRPC(&auth)
			if err != nil {
				return nil, err
			}
			defer conn.Close()
			cloudCtx, err := cloudContext(ctx, &auth)
			if err != nil {
				return nil, err
			}
			return operatorcert.Request(cloudCtx, conn, &auth, target.EntityID)
		}
	}
	return nil, fmt.Errorf("no Cloud session matches this operator certificate; sign in again")
}
