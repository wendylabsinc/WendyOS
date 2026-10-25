package enrollment

import (
	"context"
	"encoding/hex"
	"fmt"
	cloudpbv2 "github.com/wendylabsinc/WendyOS/go/wendy-core/core/enrollment/internal/cloudpb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

// DeletedAsset is retained Cloud lifecycle evidence, never an absent active row.
type DeletedAsset = cloudpbv2.DeletedAsset

// Output only: durable recovery evidence belongs to Cloud and Agent, not CLI.
type Progress struct {
	Cloud       string `json:"cloud"`
	Principal   string `json:"principal"`
	AssetID     string `json:"assetId"`
	Fingerprint string `json:"certificateSHA256,omitempty"`
	Serial      string `json:"certificateSerial,omitempty"`
	Revoked     bool   `json:"certificateRevoked"`
	Deleted     bool   `json:"assetDeleted"`
	Reset       bool   `json:"deviceReset"`
}

// AssetState retains either the active asset or authenticated deletion evidence.
type AssetState struct {
	Active  *cloudpbv2.Asset
	Deleted *cloudpbv2.DeletedAsset
}

// The authorization carries exact bytes authenticated using Cloud HTTPS keys.
// It is kept only in memory and independently verified again by the Agent.
type AuthorizedReset struct {
	Deletion []byte
	Token    []byte
}

// UnenrollOps supplies authenticated Cloud actions and host proof/reset calls.
// Authorize must verify the Cloud signature and exact operator/identity binding
// before returning. Proof returns authenticated installed-leaf revocation.
// Reset must retain keys unless the Agent confirms the reset.
type UnenrollOps struct {
	Lookup    func(context.Context, string) (*AssetState, error)
	Authorize func(context.Context, string) (*AuthorizedReset, error)
	Proof     func(context.Context) ([]byte, error)
	Reset     func(context.Context, string, string, []byte, *AuthorizedReset) error
}

// A missing active row permits only an explicit exact-asset retry. It is never
// deletion evidence: a fresh signed reset authorization is still mandatory.
func UnenrollPreflight(reply *AssetState, err error, j *Progress) error {
	if err != nil {
		if rpcCode(err) == platform.RPCNotFound && j.AssetID != "" {
			return nil
		}
		if rpcCode(err) == platform.RPCNotFound {
			return fmt.Errorf("Cloud device not found or not visible to this account; to resume an interrupted reset, supply its exact --asset-id; if Cloud has no record of this device, --force clears only its local enrollment: %w", err)
		}
		return err
	}
	if reply == nil || reply.Active == nil || reply.Deleted != nil {
		return fmt.Errorf("missing active Cloud binding")
	}
	_, err = UnenrollLifecycle(reply, j)
	return err
}

// AuthorizedDeletion validates exact deletion binding before reset admission.
func AuthorizedDeletion(a *AuthorizedReset, j *Progress) (*cloudpbv2.DeletedAsset, error) {
	if a == nil || len(a.Token) == 0 || len(a.Token) > 65536 || len(a.Deletion) == 0 || len(a.Deletion) > 4096 {
		return nil, fmt.Errorf("verified signed Cloud reset authorization required; keys retained")
	}
	var d cloudpbv2.DeletedAsset
	if err := proto.Unmarshal(a.Deletion, &d); err != nil {
		return nil, err
	}
	if _, err := UnenrollLifecycle(&AssetState{Deleted: &d}, j); err != nil {
		return nil, err
	}
	return &d, nil
}

// CheckUnenrollBinding compares the exact authenticated tenant/device/asset.
func CheckUnenrollBinding(asset *cloudpbv2.Asset, j Progress) error {
	id, err := ParsePrincipal(j.Principal)
	if err != nil || id.EntityType != EntityAsset || asset == nil || asset.GetId() != j.AssetID || asset.GetOrganizationId() != id.TenantUUID || DeviceID(asset) != id.EntityID {
		return fmt.Errorf("refusing cleanup: exact Cloud asset/tenant/PKI binding does not match the authenticated device")
	}
	return nil
}

// UnenrollLifecycle adopts an asset ID only after its exact identity matches.
func UnenrollLifecycle(reply *AssetState, j *Progress) (bool, error) {
	if reply == nil || (reply.Active != nil && reply.Deleted != nil) {
		return false, fmt.Errorf("invalid Cloud lifecycle evidence")
	}
	var asset *cloudpbv2.Asset
	deleted := false
	if reply.Deleted != nil {
		d := reply.Deleted
		if d == nil || d.GetDeletedAt() == nil || d.GetDeletedAt().CheckValid() != nil {
			return false, fmt.Errorf("invalid Cloud deletion evidence; keys retained")
		}
		asset = &cloudpbv2.Asset{Id: d.GetId(), OrganizationId: d.GetOrganizationId(), PkiDeviceName: &d.DeviceId}
		deleted = true
	} else {
		asset = reply.Active
	}
	if asset == nil {
		return false, fmt.Errorf("missing Cloud lifecycle identity")
	}
	if !canonicalUUID(asset.GetId()) {
		return false, fmt.Errorf("invalid Cloud asset UUID")
	}
	candidate := *j
	if candidate.AssetID == "" {
		candidate.AssetID = asset.GetId()
	}
	if err := CheckUnenrollBinding(asset, candidate); err != nil {
		return false, err
	}
	j.AssetID = candidate.AssetID
	return deleted, nil
}

// PerformUnenroll preserves acknowledged partial progress and refreshes Cloud
// authorization after OCSP retrieval, before the host may erase device keys.
func PerformUnenroll(ctx context.Context, j *Progress, ops UnenrollOps) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(j.Fingerprint) != 64 {
		return fmt.Errorf("verified installed certificate fingerprint required")
	}
	if _, err := hex.DecodeString(j.Fingerprint); err != nil {
		return fmt.Errorf("invalid installed certificate fingerprint")
	}
	reply, err := ops.Lookup(ctx, j.AssetID)
	if err := UnenrollPreflight(reply, err, j); err != nil {
		return fmt.Errorf("preflight Cloud asset lifecycle: %w", err)
	}
	// Request signing capability in the deleting operation itself. Modern Cloud
	// refuses before deletion if its reset signer is unavailable; an authorized
	// exact-asset retry reads the retained audit record instead of deleting again.
	authorized, err := ops.Authorize(ctx, j.AssetID)
	if err != nil {
		return fmt.Errorf("Cloud unenrollment failed/uncertain; keys retained: %w", err)
	}
	deleted, err := AuthorizedDeletion(authorized, j)
	if err != nil {
		return err
	}
	j.Deleted = true
	proof, err := ops.Proof(ctx)
	if err != nil {
		return fmt.Errorf("verifying installed-leaf PKI revocation; keys retained: %w", err)
	}
	j.Revoked = true
	// OCSP acquisition may consume the short authorization lifetime. Recheck the
	// current ACL and obtain a fresh signed authorization before local erasure.
	fresh, err := ops.Authorize(ctx, j.AssetID)
	if err != nil {
		return fmt.Errorf("refreshing Cloud reset authorization; keys retained: %w", err)
	}
	confirmed, err := AuthorizedDeletion(fresh, j)
	if err != nil {
		return err
	}
	if !proto.Equal(deleted, confirmed) {
		return fmt.Errorf("Cloud deletion evidence changed; keys retained")
	}
	if err := ops.Reset(ctx, j.Principal, j.Fingerprint, proof, fresh); err != nil {
		return fmt.Errorf("certificate revoked and asset deleted; local reset unconfirmed: %w", err)
	}
	j.Reset = true
	return nil
}
