package liteclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// legacyDeviceTLSConfig authenticates an explicitly selected legacy Cloud asset.
// Its numeric identity must be known before connecting; a hardware device ID is
// not a legacy asset ID. Tenant PKI devices use deviceTLSConfig instead.
func legacyDeviceTLSConfig(cert tls.Certificate, chain string, orgID, assetID int32) (*tls.Config, error) {
	if orgID <= 0 || assetID <= 0 {
		return nil, fmt.Errorf("legacy Wendy Lite authentication requires a positive organization and asset ID")
	}
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("missing client certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	identity, ok, err := certs.IdentityFromCert(leaf)
	if err != nil {
		return nil, err
	}
	if !ok || leaf.IsCA || identity.TenantUUID != "" || identity.EntityType != certs.EntityUser || identity.OrgID != orgID {
		return nil, fmt.Errorf("legacy Wendy Lite authentication requires a user certificate for organization %s", config.OrganizationLabel("", strconv.FormatInt(int64(orgID), 10)))
	}
	verify, err := certs.BuildServerVerifyConnection(certs.ServerVerifyOpts{
		ChainPEM: chain, ExpectedOrgID: orgID,
		ExpectedIdentity: &certs.WendyIdentity{
			OrgID: orgID, EntityType: certs.EntityAsset, EntityID: strconv.FormatInt(int64(assetID), 10),
		},
	})
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert},
		// SECURITY: Legacy certificates identify assets by Wendy URN or CN, not
		// a DNS/IP SAN. VerifyConnection replaces Go's hostname-based verifier
		// with trusted-chain, validity, server-auth and exact org/asset checks.
		// Keep it installed whenever InsecureSkipVerify is true. See
		// https://pkg.go.dev/crypto/tls#example-Config-VerifyConnection.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if err := verify(state); err != nil {
				return err
			}
			peer, _, err := certs.IdentityFromCert(state.PeerCertificates[0])
			if err != nil {
				return err
			}
			if peer.TenantUUID != "" {
				return fmt.Errorf("legacy Wendy Lite authentication cannot accept a tenant PKI device")
			}
			return nil
		},
	}, nil
}

// ConnectWithLegacyAuthenticationContext verifies a legacy Cloud asset using
// its numeric organization and asset IDs. TLS 1.2 remains available for older
// firmware without changing the tenant PKI or Cloud relay authentication paths.
func (c *WendyLiteClient) ConnectWithLegacyAuthenticationContext(ctx context.Context, address string, cert tls.Certificate, chain string, orgID, assetID int32) error {
	cfg, err := legacyDeviceTLSConfig(cert, chain, orgID, assetID)
	if err != nil {
		return err
	}
	return c.connectTLSContext(ctx, address, cfg)
}
