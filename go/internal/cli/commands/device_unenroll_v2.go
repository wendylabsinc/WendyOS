package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
)

// Output only: durable recovery evidence belongs to Cloud and Agent, not CLI.
type v2UnenrollProgress struct {
	Cloud       string `json:"cloud"`
	Principal   string `json:"principal"`
	AssetID     string `json:"assetId"`
	Fingerprint string `json:"certificateSHA256,omitempty"`
	Serial      string `json:"certificateSerial,omitempty"`
	Revoked     bool   `json:"certificateRevoked"`
	Deleted     bool   `json:"assetDeleted"`
	Reset       bool   `json:"deviceReset"`
}

func isLegacyCloudEnrollment(orgID, assetID int32) bool { return orgID > 0 && assetID > 0 }

type v2UnenrollOps struct {
	lookup func(context.Context, string) (*cloudpbv2.GetAssetLifecycleResponse, error)
	revoke func(context.Context, string, string) (*agentpbv2.RevokeACMECertificateResponse, error)
	delete func(context.Context, string) error
	reset  func(context.Context, string, string) error
}

func checkV2UnenrollBinding(asset *cloudpbv2.Asset, j v2UnenrollProgress) error {
	id, err := certs.ParsePrincipal(j.Principal)
	if err != nil || id.EntityType != certs.EntityAsset || asset == nil || asset.GetId() != j.AssetID || asset.GetOrganizationId() != id.TenantUUID || asset.GetPkiDeviceName() != id.EntityID {
		return fmt.Errorf("refusing cleanup: exact Cloud asset/tenant/PKI binding does not match the authenticated device")
	}
	return nil
}

func v2UnenrollLifecycle(reply *cloudpbv2.GetAssetLifecycleResponse, j *v2UnenrollProgress) (bool, error) {
	var asset *cloudpbv2.Asset
	deleted := false
	switch state := reply.GetState().(type) {
	case *cloudpbv2.GetAssetLifecycleResponse_Active:
		asset = state.Active
	case *cloudpbv2.GetAssetLifecycleResponse_Deleted:
		d := state.Deleted
		if d == nil || d.GetDeletedAt() == nil || d.GetDeletedAt().CheckValid() != nil {
			return false, fmt.Errorf("invalid Cloud deletion evidence; keys retained")
		}
		asset = &cloudpbv2.Asset{Id: d.GetId(), OrganizationId: d.GetOrganizationId(), PkiDeviceName: &d.PkiDeviceName}
		deleted = true
	default:
		return false, fmt.Errorf("Cloud asset lifecycle is unknown; no deletion evidence; keys retained")
	}
	if asset == nil {
		return false, fmt.Errorf("missing Cloud lifecycle identity")
	}
	parsed, err := uuid.Parse(asset.GetId())
	if err != nil || parsed.String() != asset.GetId() {
		return false, fmt.Errorf("invalid Cloud asset UUID")
	}
	candidate := *j
	if candidate.AssetID == "" {
		candidate.AssetID = asset.GetId()
	}
	if err := checkV2UnenrollBinding(asset, candidate); err != nil {
		return false, err
	}
	j.AssetID = candidate.AssetID
	return deleted, nil
}

func performV2Unenroll(ctx context.Context, j *v2UnenrollProgress, ops v2UnenrollOps) error {
	if len(j.Fingerprint) != 64 {
		return fmt.Errorf("verified installed certificate fingerprint required")
	}
	if _, err := hex.DecodeString(j.Fingerprint); err != nil {
		return fmt.Errorf("invalid installed certificate fingerprint")
	}
	reply, err := ops.lookup(ctx, j.AssetID)
	if err != nil {
		return fmt.Errorf("preflight Cloud asset lifecycle: %w", err)
	}
	if _, err := v2UnenrollLifecycle(reply, j); err != nil {
		return err
	}
	// Always obtain the Agent's durable acknowledgement, including on retry.
	ack, err := ops.revoke(ctx, j.Principal, j.Fingerprint)
	if err != nil {
		return fmt.Errorf("revoking installed ACME certificate; no keys reset: %w", err)
	}
	if ack.GetPrincipalUri() != j.Principal || ack.GetCertificateSha256() != j.Fingerprint || ack.GetCertificateSerial() == "" {
		return fmt.Errorf("invalid revocation acknowledgement; no deletion or reset")
	}
	j.Revoked = true
	j.Serial = ack.GetCertificateSerial()
	// Revalidate authoritative binding after potentially slow revocation.
	reply, err = ops.lookup(ctx, j.AssetID)
	if err != nil {
		return fmt.Errorf("rechecking Cloud lifecycle; keys retained: %w", err)
	}
	deleted, err := v2UnenrollLifecycle(reply, j)
	if err != nil {
		return err
	}
	if !deleted {
		if err := ops.delete(ctx, j.AssetID); err != nil {
			return fmt.Errorf("certificate revoked; Cloud deletion failed/uncertain; keys retained: %w", err)
		}
	}
	// A successful delete response alone is not retained binding evidence.
	reply, err = ops.lookup(ctx, j.AssetID)
	if err != nil {
		return fmt.Errorf("confirming Cloud tombstone; keys retained: %w", err)
	}
	deleted, err = v2UnenrollLifecycle(reply, j)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("Cloud asset still active; reset refused")
	}
	j.Deleted = true
	if err := ops.reset(ctx, j.Principal, j.Fingerprint); err != nil {
		return fmt.Errorf("certificate revoked and asset deleted; local reset unconfirmed: %w", err)
	}
	j.Reset = true
	return nil
}

func directUnenrollPeerFingerprint(conn *grpcclient.AgentConnection, verifiedPeer *grpcpeer.Peer, principal certs.WendyIdentity) (string, error) {
	if conn == nil || !conn.IsMTLS || conn.IsSessionProxy || verifiedPeer == nil {
		return "", fmt.Errorf("v2 unenroll requires a directly verified mTLS device connection; use its LAN hostname/IP")
	}
	tlsInfo, ok := verifiedPeer.AuthInfo.(credentials.TLSInfo)
	if !ok || !tlsInfo.State.HandshakeComplete || len(tlsInfo.State.PeerCertificates) == 0 {
		return "", fmt.Errorf("verified peer certificate unavailable; no mutation performed")
	}
	leaf := tlsInfo.State.PeerCertificates[0]
	peerPrincipal, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok || peerPrincipal != principal.Principal {
		return "", fmt.Errorf("Agent state disagrees with its verified TLS identity")
	}
	digest := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(digest[:]), nil
}

func runV2DeviceUnenroll(ctx context.Context, conn *grpcclient.AgentConnection, prov *agentpbv2.ProvisionedResponse, verifiedPeer *grpcpeer.Peer, override, assetOverride string, yes, checkOnly bool) error {
	principal, err := certs.ParsePrincipal(prov.GetPrincipalUri())
	if err != nil || principal.EntityType != certs.EntityAsset {
		return fmt.Errorf("invalid direct PKI device identity")
	}
	fingerprint, err := directUnenrollPeerFingerprint(conn, verifiedPeer, principal)
	if err != nil {
		return err
	}
	cloud := prov.GetCloudHost()
	if cloud == "" || (override != "" && override != cloud) {
		return fmt.Errorf("cleanup endpoint must match the device's enrolled Cloud host")
	}
	auth, err := pickAuthEntry(cloud)
	if err != nil {
		return err
	}
	auth, err = prepareEnrollmentAuth(ctx, auth)
	if err != nil {
		return err
	}
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != principal.TenantUUID {
		return fmt.Errorf("cleanup operator tenant does not match the device")
	}
	rpcctx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer cloudConn.Close()
	assets := cloudpbv2.NewAssetServiceClient(cloudConn)
	j := v2UnenrollProgress{Cloud: cloud, Principal: principal.Principal, AssetID: assetOverride, Fingerprint: fingerprint}
	if assetOverride != "" {
		id, err := uuid.Parse(assetOverride)
		if err != nil || id.String() != assetOverride {
			return fmt.Errorf("--asset-id requires a canonical UUID")
		}
	}
	svc := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn)
	ops := v2UnenrollOps{
		lookup: func(ctx context.Context, id string) (*cloudpbv2.GetAssetLifecycleResponse, error) {
			req := &cloudpbv2.GetAssetLifecycleRequest{OrganizationId: principal.TenantUUID, PkiDeviceName: principal.EntityID}
			if id != "" {
				req.AssetId = &id
			}
			return assets.GetAssetLifecycle(rpcctx, req)
		},
		revoke: func(ctx context.Context, p, fp string) (*agentpbv2.RevokeACMECertificateResponse, error) {
			return svc.RevokeACMECertificate(ctx, &agentpbv2.RevokeACMECertificateRequest{ExpectedPrincipalUri: p, ExpectedCertificateSha256: fp})
		},
		delete: func(ctx context.Context, id string) error {
			reply := &cloudpbv2.DeleteAssetResponse{}
			expected := principal.EntityID
			err := cloudrequest.Invoke(rpcctx, cloudConn, auth, cloudpbv2.AssetService_DeleteAsset_FullMethodName, &cloudpbv2.DeleteAssetRequest{Id: id, ExpectedPkiDeviceName: &expected}, reply)
			if err == nil && !reply.GetSuccess() {
				return fmt.Errorf("Cloud deletion was not acknowledged")
			}
			return err
		},
		reset: func(ctx context.Context, p, fp string) error {
			_, err := svc.Unprovision(ctx, &agentpbv2.UnprovisionRequest{ExpectedPrincipalUri: p, ExpectedCertificateSha256: fp})
			return err
		},
	}
	reply, err := ops.lookup(ctx, j.AssetID)
	if err != nil {
		return fmt.Errorf("reading Cloud lifecycle (updated Cloud required): %w", err)
	}
	if _, err := v2UnenrollLifecycle(reply, &j); err != nil {
		return err
	}
	if checkOnly {
		_, probeErr := svc.CheckACMERevocation(ctx, &agentpbv2.RevokeACMECertificateRequest{ExpectedPrincipalUri: j.Principal, ExpectedCertificateSha256: j.Fingerprint})
		if jsonOutput {
			out, _ := json.Marshal(map[string]any{"principal": j.Principal, "accountLookupReady": probeErr == nil, "revocationAttempted": false, "assetDeleted": false, "deviceReset": false})
			fmt.Println(string(out))
		} else if probeErr == nil {
			fmt.Println("Existing ACME account lookup succeeded. No revocation, deletion or reset attempted.")
		}
		return probeErr
	}
	if !yes {
		if !isInteractiveTerminal() {
			return fmt.Errorf("unenroll is destructive; pass --yes to confirm")
		}
		fmt.Printf("Revoke installed certificate for %s, delete Cloud asset %s at %s, then reset this device.\n", j.Principal, j.AssetID, j.Cloud)
		if !confirmDefaultNoFn("Continue?") {
			return nil
		}
	}
	err = performV2Unenroll(ctx, &j, ops)
	if jsonOutput {
		out, _ := json.MarshalIndent(j, "", "  ")
		fmt.Println(string(out))
	}
	if err != nil {
		return err
	}
	if err := config.Update(func(cfg *config.Config) (bool, error) { clearPinsForIdentity(cfg, principal); return true, nil }); err != nil {
		return fmt.Errorf("unenrolled; local identity pin cleanup failed: %w", err)
	}
	if !jsonOutput {
		fmt.Printf("Revoked installed certificate, deleted asset %s, and reset the device.\n", j.AssetID)
	}
	return nil
}
