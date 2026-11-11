// Package operatorcert obtains ephemeral device-scoped credentials through
// Cloud's existing signed issuance route. It never persists an issued key.
package operatorcert

import (
	"bytes"
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

func Request(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, deviceUUID string) (*config.CertificateInfo, error) {
	if auth == nil || len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("sign in before requesting operator permissions")
	}
	if _, err := meshid.ParseDeviceUUID(deviceUUID); err != nil {
		return nil, fmt.Errorf("entitled certificate requires a canonical Cloud device UUID: %w", err)
	}
	login := auth.Certificates[0]
	leaves, err := certs.ParseCertsFromPEM([]byte(login.PemCertificate))
	if err != nil || len(leaves) != 1 {
		return nil, fmt.Errorf("invalid login certificate")
	}
	identity, found, err := certs.IdentityFromCert(leaves[0])
	if err != nil || !found || identity.EntityType != certs.EntityUser || identity.Principal == "" {
		return nil, fmt.Errorf("Cloud operator principal required")
	}
	key, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		return nil, err
	}
	csr, err := certs.GenerateCSR([]byte(key), "", []string{identity.Principal})
	if err != nil {
		return nil, err
	}
	req := &cloudpbv2.RequestEntitledCertificateRequest{OrganizationId: identity.TenantUUID, CsrPem: csr, TargetType: cloudpbv2.AccessTargetType_ACCESS_TARGET_TYPE_DEVICE, TargetRef: deviceUUID}
	resp := new(cloudpbv2.RequestEntitledCertificateResponse)
	if err := cloudrequest.Invoke(ctx, conn, auth, cloudpbv2.OperatorCertificateService_RequestEntitledCertificate_FullMethodName, req, resp); err != nil {
		return nil, fmt.Errorf("requesting device operator permissions: %w", err)
	}
	return Validate(login, key, resp, deviceUUID, time.Now())
}

// Validate binds issuance to the fresh key, original identity, trusted CA chain,
// requested device, and validity window. Response metadata is not authority.
func Validate(login config.CertificateInfo, key string, resp *cloudpbv2.RequestEntitledCertificateResponse, deviceUUID string, now time.Time) (*config.CertificateInfo, error) {
	leaves, err := certs.ParseCertsFromPEM([]byte(resp.GetCertificatePem()))
	if err != nil || len(leaves) != 1 {
		return nil, fmt.Errorf("Cloud returned an invalid entitled leaf")
	}
	leaf := leaves[0]
	original, err := certs.ParseCertsFromPEM([]byte(login.PemCertificate))
	if err != nil || len(original) != 1 {
		return nil, fmt.Errorf("invalid login certificate")
	}
	if len(leaf.URIs) != 1 || len(original[0].URIs) != 1 || leaf.URIs[0].String() != original[0].URIs[0].String() {
		return nil, fmt.Errorf("entitled certificate changed operator principal")
	}
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(key))
	if err != nil {
		return nil, err
	}
	public, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(public, leaf.RawSubjectPublicKeyInfo) || bytes.Equal(public, original[0].RawSubjectPublicKeyInfo) {
		return nil, fmt.Errorf("entitled certificate does not bind the fresh CSR key")
	}
	peers, err := certs.ParseCertsFromPEM([]byte(resp.GetChainPem()))
	if err != nil {
		return nil, err
	}
	roots, err := certs.ParseCertsFromPEM([]byte(login.PemCertificateChain))
	if err != nil {
		return nil, err
	}
	if err := certs.VerifyPeerCertificateChain(leaf, peers, roots, x509.ExtKeyUsageClientAuth, now, now); err != nil {
		return nil, fmt.Errorf("verifying entitled certificate: %w", err)
	}
	if leaf.NotAfter.Sub(leaf.NotBefore) > 15*time.Minute || !leaf.NotAfter.After(now.Add(30*time.Second)) {
		return nil, fmt.Errorf("invalid entitled certificate lifetime")
	}
	claims, present, err := operatorpermission.Parse(leaf)
	if err != nil || !present || len(claims) == 0 {
		return nil, fmt.Errorf("missing or malformed certificate entitlements")
	}
	for _, claim := range claims {
		if claim.Target != "device:"+deviceUUID {
			return nil, fmt.Errorf("issued entitlement does not match requested device")
		}
	}
	return &config.CertificateInfo{PemCertificate: resp.GetCertificatePem(), PemCertificateChain: resp.GetChainPem() + "\n" + login.PemCertificateChain, PemPrivateKey: key, PrincipalURI: leaf.URIs[0].String(), OrganizationID: login.OrganizationID, UserID: login.UserID}, nil
}
