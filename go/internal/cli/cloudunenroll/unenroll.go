// Package cloudunenroll removes a cloud device enrollment without contacting
// the device. Bootstrap credentials are revoked before issued certificates.
package cloudunenroll

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/pkicorepb"
	"google.golang.org/grpc"
)

// Result records only operations acknowledged by Cloud. An error may accompany
// partial progress; the caller must not report a completed unenrollment then.
type Result struct {
	CertificatesRevoked int
	CredentialsRevoked  int
	AssetDeleted        bool
}

// DeviceID is the device UUID the asset's certificates name. Since cloud #873
// that is the asset id. Older rows, and rows the re-key parked, record it as
// pki_device_name instead, which then differs from the id.
func DeviceID(asset *cloudpb.Asset) string {
	if name := asset.GetPkiDeviceName(); name != "" {
		return name
	}
	return asset.GetId()
}

// Remove validates the selected asset, revokes its unused bootstrap credentials
// and issued certificates, then deletes the asset. No device connection is used.
func Remove(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, asset *cloudpb.Asset) (result Result, err error) {
	if auth == nil || len(auth.Certificates) == 0 || asset == nil {
		return result, fmt.Errorf("an operator session and cloud asset are required")
	}
	tenant, deviceID, assetID := auth.Certificates[0].TenantUUID(), DeviceID(asset), asset.GetId()
	if id, parseErr := uuid.Parse(assetID); parseErr != nil || id.String() != assetID || id == uuid.Nil {
		return result, fmt.Errorf("asset ID must be a canonical nonzero UUID")
	}
	if tenant == "" || tenant != asset.GetOrganizationId() || deviceID == "" {
		return result, fmt.Errorf("asset does not carry a device binding in the selected operator's tenant")
	}
	assets := cloudpb.NewAssetServiceClient(conn)
	checkBinding := func() error {
		current, lookupErr := assets.GetAsset(ctx, &cloudpb.GetAssetRequest{Id: assetID})
		if lookupErr != nil {
			return fmt.Errorf("checking cloud asset binding: %w", lookupErr)
		}
		if current.GetId() != assetID || current.GetOrganizationId() != tenant || DeviceID(current) != deviceID {
			return fmt.Errorf("cloud asset binding changed; refusing to remove a different enrollment")
		}
		return nil
	}
	if err := checkBinding(); err != nil {
		return result, err
	}

	pki := cloudpb.NewPKIAdminServiceClient(conn)
	tokens, err := enrollmentTokens(ctx, pki, tenant, deviceID)
	if err != nil {
		return result, err
	}
	keys, err := eabKeys(ctx, pki, tenant, deviceID)
	if err != nil {
		return result, err
	}
	for _, tokenID := range tokens {
		err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.PKIAdminService_RevokeEnrollmentToken_FullMethodName,
			&cloudpb.RevokeEnrollmentTokenRequest{OrganizationId: tenant, TokenId: tokenID}, &pkicorepb.RevokeEnrollmentTokenResponse{})
		if err != nil {
			return result, fmt.Errorf("revoking device enrollment token: %w", err)
		}
		result.CredentialsRevoked++
	}
	for _, keyID := range keys {
		err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.PKIAdminService_RevokeEABKey_FullMethodName,
			&cloudpb.RevokeEABKeyRequest{OrganizationId: tenant, KeyId: keyID}, &pkicorepb.RevokeEABKeyResponse{})
		if err != nil {
			return result, fmt.Errorf("revoking device EAB credential: %w", err)
		}
		result.CredentialsRevoked++
	}
	result.CertificatesRevoked, err = cloudrequest.RevokePrincipalCertificates(ctx, conn, auth, deviceID)
	if err != nil {
		return result, fmt.Errorf("revoking device certificates: %w", err)
	}
	if err := checkBinding(); err != nil {
		return result, err
	}
	deleted := &cloudpb.DeleteAssetResponse{}
	if err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.AssetService_DeleteAsset_FullMethodName,
		&cloudpb.DeleteAssetRequest{Id: assetID, ExpectedDeviceId: &deviceID}, deleted); err != nil {
		return result, fmt.Errorf("deleting cloud asset: %w", err)
	}
	if !deleted.GetSuccess() {
		return result, fmt.Errorf("cloud did not confirm asset deletion")
	}
	result.AssetDeleted = true
	return result, nil
}

// Complete both inventories before mutation. Bound pagination and reject a
// repeated cursor instead of looping indefinitely against a faulty server.
const maxCredentials = 10000

func enrollmentTokens(ctx context.Context, pki cloudpb.PKIAdminServiceClient, tenant, deviceID string) ([]string, error) {
	var ids []string
	seenPages, seenIDs := map[string]bool{}, map[string]bool{}
	page, count := "", 0
	for {
		if seenPages[page] {
			return nil, fmt.Errorf("cloud repeated an enrollment-token page")
		}
		seenPages[page] = true
		response, err := pki.ListEnrollmentTokens(ctx, &cloudpb.ListEnrollmentTokensRequest{
			OrganizationId: tenant, PageSize: 200, PageToken: page, ShowConsumed: true,
		})
		if err != nil {
			return nil, fmt.Errorf("listing device enrollment credentials: %w", err)
		}
		count += len(response.GetTokens())
		if count > maxCredentials || len(seenPages) > maxCredentials {
			return nil, fmt.Errorf("cloud enrollment-token inventory exceeds %d entries", maxCredentials)
		}
		for _, token := range response.GetTokens() {
			if token.GetDeviceId() != deviceID || token.GetConsumed() || token.GetRevokedAt() != "" {
				continue
			}
			id := token.GetTokenId()
			if id == "" {
				return nil, fmt.Errorf("cloud returned an enrollment credential without an ID")
			}
			if !seenIDs[id] {
				ids = append(ids, id)
				seenIDs[id] = true
			}
		}
		page = response.GetNextPageToken()
		if page == "" {
			return ids, nil
		}
	}
}

func eabKeys(ctx context.Context, pki cloudpb.PKIAdminServiceClient, tenant, deviceID string) ([]string, error) {
	var ids []string
	seenPages, seenIDs := map[string]bool{}, map[string]bool{}
	page, count := "", 0
	for {
		if seenPages[page] {
			return nil, fmt.Errorf("cloud repeated an EAB-key page")
		}
		seenPages[page] = true
		response, err := pki.ListEABKeys(ctx, &cloudpb.ListEABKeysRequest{
			OrganizationId: tenant, PageSize: 200, PageToken: page,
		})
		if err != nil {
			return nil, fmt.Errorf("listing device EAB credentials: %w", err)
		}
		count += len(response.GetKeys())
		if count > maxCredentials || len(seenPages) > maxCredentials {
			return nil, fmt.Errorf("cloud EAB-key inventory exceeds %d entries", maxCredentials)
		}
		for _, key := range response.GetKeys() {
			if key.GetDeviceId() != deviceID || key.GetStatus() == "consumed" || key.GetStatus() == "revoked" {
				continue
			}
			if key.GetStatus() != "active" || key.GetKeyId() == "" {
				return nil, fmt.Errorf("cloud returned a device EAB credential with unknown status or missing ID")
			}
			id := key.GetKeyId()
			if !seenIDs[id] {
				ids = append(ids, id)
				seenIDs[id] = true
			}
		}
		page = response.GetNextPageToken()
		if page == "" {
			return ids, nil
		}
	}
}
