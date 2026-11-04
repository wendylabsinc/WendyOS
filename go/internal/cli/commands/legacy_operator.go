package commands

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// legacyOperatorForOrg requires the numeric operator binding. A transitional
// SPIFFE SAN may accompany it; a PKI-only identity cannot borrow numeric config
// fields to enter the legacy path.
func legacyOperatorForOrg(cert config.CertificateInfo, org int32) bool {
	if org <= 0 || cert.OrganizationID != int(org) || cert.UserID == "" || cert.AssetID != 0 {
		return false
	}
	if cert.PemCertificate == "" {
		return cert.TenantUUID() == ""
	}
	leaves, err := certs.ParseCertsFromPEM([]byte(cert.PemCertificate))
	if err != nil || len(leaves) == 0 {
		return false
	}
	identity, found, err := certs.IdentityFromCert(leaves[0])
	return err == nil && found && identity.LegacyURN() == certs.UserURN(org, cert.UserID) && (cert.PrincipalURI == "" || cert.PrincipalURI == identity.Principal)
}
