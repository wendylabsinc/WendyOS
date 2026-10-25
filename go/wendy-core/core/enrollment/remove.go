package enrollment

import (
	"context"
	"fmt"

	cloudpb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	pkicorepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/pkipb"
)

// Asset is the selected Cloud device and its immutable tenant/device binding.
type Asset = cloudpb.Asset

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
func (c *Client) Remove(ctx context.Context, session Session, asset *Asset) (result Result, err error) {
	if session.Principal == "" || asset == nil {
		return result, fmt.Errorf("an operator session and cloud asset are required")
	}
	tenant, deviceID, assetID := operatorTenant(session.Principal), DeviceID(asset), asset.GetId()
	if !canonicalUUID(assetID) || assetID == "00000000-0000-0000-0000-000000000000" {
		return result, fmt.Errorf("asset ID must be a canonical nonzero UUID")
	}
	if tenant == "" || tenant != asset.GetOrganizationId() || deviceID == "" {
		return result, fmt.Errorf("asset does not carry a device binding in the selected operator's tenant")
	}

	checkBinding := func() error {
		current := new(Asset)
		lookupErr := unary(ctx, c.Cloud, "/wendycloud.v2.AssetService/GetAsset", &cloudpb.GetAssetRequest{Id: assetID}, current)
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

	tokens, err := c.enrollmentTokens(ctx, tenant, deviceID)
	if err != nil {
		return result, err
	}
	keys, err := c.eabKeys(ctx, tenant, deviceID)
	if err != nil {
		return result, err
	}
	for _, tokenID := range tokens {
		_, err := c.signed(ctx, "/wendycloud.v2.PKIAdminService/RevokeEnrollmentToken",
			&cloudpb.RevokeEnrollmentTokenRequest{OrganizationId: tenant, TokenId: tokenID}, &pkicorepb.RevokeEnrollmentTokenResponse{}, "")
		if err != nil {
			return result, fmt.Errorf("revoking device enrollment token: %w", err)
		}
		result.CredentialsRevoked++
	}
	for _, keyID := range keys {
		_, err := c.signed(ctx, "/wendycloud.v2.PKIAdminService/RevokeEABKey",
			&cloudpb.RevokeEABKeyRequest{OrganizationId: tenant, KeyId: keyID}, &pkicorepb.RevokeEABKeyResponse{}, "")
		if err != nil {
			return result, fmt.Errorf("revoking device EAB credential: %w", err)
		}
		result.CredentialsRevoked++
	}
	result.CertificatesRevoked, err = c.revokePrincipal(ctx, tenant, deviceID)
	if err != nil {
		return result, fmt.Errorf("revoking device certificates: %w", err)
	}
	if err := checkBinding(); err != nil {
		return result, err
	}
	deleted := &cloudpb.DeleteAssetResponse{}
	if _, err := c.signed(ctx, "/wendycloud.v2.AssetService/DeleteAsset",
		&cloudpb.DeleteAssetRequest{Id: assetID, ExpectedDeviceId: &deviceID}, deleted, ""); err != nil {
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

func (c *Client) enrollmentTokens(ctx context.Context, tenant, deviceID string) ([]string, error) {
	var ids []string
	seenPages, seenIDs := map[string]bool{}, map[string]bool{}
	page, count := "", 0
	for {
		if seenPages[page] {
			return nil, fmt.Errorf("cloud repeated an enrollment-token page")
		}
		seenPages[page] = true
		response := new(pkicorepb.ListEnrollmentTokensResponse)
		err := unary(ctx, c.Cloud, "/wendycloud.v2.PKIAdminService/ListEnrollmentTokens", &cloudpb.ListEnrollmentTokensRequest{
			OrganizationId: tenant, PageSize: 200, PageToken: page, ShowConsumed: true,
		}, response)
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

func (c *Client) eabKeys(ctx context.Context, tenant, deviceID string) ([]string, error) {
	var ids []string
	seenPages, seenIDs := map[string]bool{}, map[string]bool{}
	page, count := "", 0
	for {
		if seenPages[page] {
			return nil, fmt.Errorf("cloud repeated an EAB-key page")
		}
		seenPages[page] = true
		response := new(pkicorepb.ListEABKeysResponse)
		err := unary(ctx, c.Cloud, "/wendycloud.v2.PKIAdminService/ListEABKeys", &cloudpb.ListEABKeysRequest{
			OrganizationId: tenant, PageSize: 200, PageToken: page,
		}, response)
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
