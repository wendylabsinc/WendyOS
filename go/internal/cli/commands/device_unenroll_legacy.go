package commands

import (
	"context"
	"errors"
	"fmt"
	"io"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type legacyUnenrollResult struct {
	DeviceReset         bool
	CertificatesRevoked int
	AssetDeleted        bool
}

// Check Cloud before erasing the only local record of the enrollment. Keep
// reset before revocation, since Cloud cleanup can disrupt device connectivity.
func performLegacyDeviceUnenroll(ctx context.Context, conn grpc.ClientConnInterface, orgID, assetID int32, userID string, reset func() error) (result legacyUnenrollResult, err error) {
	if err := checkLegacyUnenrollRole(ctx, conn, orgID, userID); err != nil {
		return result, fmt.Errorf("cloud cleanup preflight failed; device was not reset: %w", err)
	}
	if _, err := listLegacyUnenrollCertificates(ctx, conn, assetID); err != nil {
		return result, fmt.Errorf("cloud cleanup preflight failed; device was not reset: %w", err)
	}
	if err := reset(); err != nil {
		if status.Code(err) != codes.Unavailable {
			return result, fmt.Errorf("unprovisioning device: %w", err)
		}
		cliLogln("Device connection closed (agent is restarting).")
	}
	result.DeviceReset = true
	// Read again after reset to include certificates issued during preflight.
	result.CertificatesRevoked, result.AssetDeleted, err = cloudUnenrollCleanupClient(ctx, conn, assetID)
	return result, err
}

// Legacy certificate listing only checks membership and can return an empty
// inventory for non-members. Asset deletion requires an owner or admin, so check
// the user from the selected Cloud certificate before any reset or revocation.
func checkLegacyUnenrollRole(ctx context.Context, conn grpc.ClientConnInterface, orgID int32, userID string) error {
	if userID == "" {
		return fmt.Errorf("selected cloud auth session has no user ID; re-run 'wendy auth login' with an owner or admin of organization %d", orgID)
	}
	client := cloudpb.NewOrganizationServiceClient(conn)
	seenTokens := make(map[string]bool)
	for pageToken := ""; ; {
		response, err := client.ListMembers(ctx, &cloudpb.ListMembersRequest{OrganizationId: orgID, PageToken: pageToken})
		if err != nil {
			return fmt.Errorf("checking owner or admin membership in organization %d: %w", orgID, err)
		}
		for _, member := range response.GetMembers() {
			if member.GetUserId() != userID || member.GetOrganizationId() != orgID {
				continue
			}
			switch member.GetRole() {
			case cloudpb.OrganizationRole_ORGANIZATION_ROLE_OWNER, cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN:
				return nil
			default:
				return fmt.Errorf("unenrolling this device requires an owner or admin of organization %d; selected user has role %s", orgID, member.GetRole())
			}
		}
		pageToken = response.GetNextPageToken()
		if pageToken == "" {
			return fmt.Errorf("selected user is not listed as a member of organization %d; sign in as an owner or admin of that organization", orgID)
		}
		if seenTokens[pageToken] {
			return fmt.Errorf("cloud membership inventory repeats a page token for organization %d; cannot verify owner or admin membership", orgID)
		}
		seenTokens[pageToken] = true
	}
}

const maxLegacyUnenrollCertificates = 10000

// Consume every page before mutation. The legacy server defaults to just 20
// records, and stream creation alone does not establish a successful RPC.
func listLegacyUnenrollCertificates(ctx context.Context, conn grpc.ClientConnInterface, assetID int32) ([]int32, error) {
	client := cloudpb.NewCertificateServiceClient(conn)
	var active []int32
	seen := make(map[int32]bool)
	var total int32
	for offset := int32(0); ; {
		stream, err := client.ListCertificates(ctx, &cloudpb.ListCertificatesRequest{
			AssetId: assetID, Offset: int32Ptr(offset), Limit: int32Ptr(200),
		})
		if err != nil {
			return nil, fmt.Errorf("listing certificates for asset %d: %w", assetID, err)
		}
		var count int32
		for {
			response, err := stream.Recv()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("listing certificates for asset %d: %w", assetID, err)
			}
			total = max(total, response.GetTotal())
			certificate := response.GetCertificate()
			if certificate == nil {
				continue
			}
			id := certificate.GetId()
			if id <= 0 || seen[id] {
				return nil, fmt.Errorf("cloud certificate inventory contains a missing or repeated ID; retry unenrollment")
			}
			if certificate.AssetId != nil && certificate.GetAssetId() != assetID {
				return nil, fmt.Errorf("cloud certificate inventory contains a certificate for another asset")
			}
			if len(seen) >= maxLegacyUnenrollCertificates || total > maxLegacyUnenrollCertificates {
				return nil, fmt.Errorf("cloud certificate inventory exceeds %d entries", maxLegacyUnenrollCertificates)
			}
			seen[id] = true
			count++
			if certificate.GetStatus() == cloudpb.CertificateStatus_CERTIFICATE_STATUS_ACTIVE {
				active = append(active, id)
			}
		}
		offset += count
		if count == 0 && offset < total {
			return nil, fmt.Errorf("cloud certificate inventory ended before all certificates were returned; retry unenrollment")
		}
		if offset >= total {
			return active, nil
		}
	}
}

func cloudUnenrollCleanupClient(ctx context.Context, conn grpc.ClientConnInterface, assetID int32) (certsRevoked int, assetDeleted bool, err error) {
	certificates, err := listLegacyUnenrollCertificates(ctx, conn, assetID)
	if err != nil {
		return 0, false, err
	}
	client := cloudpb.NewCertificateServiceClient(conn)
	for _, id := range certificates {
		revoked, err := client.RevokeCertificate(ctx, &cloudpb.RevokeCertificateRequest{
			CertificateId: id, Reason: "device unprovisioned",
		})
		if err != nil {
			return certsRevoked, false, fmt.Errorf("revoking certificate %d: %w", id, err)
		}
		if !revoked.GetSuccess() {
			return certsRevoked, false, fmt.Errorf("cloud did not confirm revocation of certificate %d", id)
		}
		certsRevoked++
	}
	deleted, err := cloudpb.NewAssetServiceClient(conn).DeleteAsset(ctx, &cloudpb.DeleteAssetRequest{Id: assetID})
	if err != nil {
		return certsRevoked, false, fmt.Errorf("deleting asset %d: %w", assetID, err)
	}
	if !deleted.GetSuccess() {
		return certsRevoked, false, fmt.Errorf("cloud did not confirm deletion of asset %d", assetID)
	}
	return certsRevoked, true, nil
}
