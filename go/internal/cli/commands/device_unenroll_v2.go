package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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

type v2AssetState struct {
	Active  *cloudpbv2.Asset
	Deleted *cloudpbv2.DeletedAsset
}

// A status code or message alone is never deletion evidence.
func v2AssetLookupResult(asset *cloudpbv2.Asset, err error) (*v2AssetState, error) {
	if err == nil {
		return &v2AssetState{Active: asset}, nil
	}
	if status.Code(err) != codes.NotFound {
		return nil, err
	}
	var deleted *cloudpbv2.DeletedAsset
	for _, detail := range status.Convert(err).Details() {
		if d, ok := detail.(*cloudpbv2.DeletedAsset); ok {
			if deleted != nil {
				return nil, fmt.Errorf("ambiguous Cloud deletion details")
			}
			deleted = d
		}
	}
	if deleted == nil {
		return nil, fmt.Errorf("Cloud asset unknown; no retained deletion proof: %w", err)
	}
	return &v2AssetState{Deleted: deleted}, nil
}

type v2UnenrollOps struct {
	lookup func(context.Context, string) (*v2AssetState, error)
	delete func(context.Context, string) error
	proof  func(context.Context) ([]byte, error)
	reset  func(context.Context, string, string, []byte, *cloudpbv2.DeletedAsset) error
}

func checkV2UnenrollBinding(asset *cloudpbv2.Asset, j v2UnenrollProgress) error {
	id, err := certs.ParsePrincipal(j.Principal)
	if err != nil || id.EntityType != certs.EntityAsset || asset == nil || asset.GetId() != j.AssetID || asset.GetOrganizationId() != id.TenantUUID || asset.GetPkiDeviceName() != id.EntityID {
		return fmt.Errorf("refusing cleanup: exact Cloud asset/tenant/PKI binding does not match the authenticated device")
	}
	return nil
}

func v2UnenrollLifecycle(reply *v2AssetState, j *v2UnenrollProgress) (bool, error) {
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
	// Cloud owns the only revocation/deletion workflow. Every retry reads its
	// durable binding, never a CLI journal or a previous transient response.
	deleted, err := v2UnenrollLifecycle(reply, j)
	if err != nil {
		return err
	}
	if !deleted {
		if err := ops.delete(ctx, j.AssetID); err != nil {
			return fmt.Errorf("Cloud unenrollment failed/uncertain; keys retained: %w", err)
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
	proof, err := ops.proof(ctx)
	if err != nil {
		return fmt.Errorf("verifying installed-leaf PKI revocation; keys retained: %w", err)
	}
	j.Revoked = true
	if err := ops.reset(ctx, j.Principal, j.Fingerprint, proof, reply.Deleted); err != nil {
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
	if !prov.GetCloudUnenrollmentSupported() {
		return fmt.Errorf("updated Agent with Cloud-owned reset/recovery support required; no mutation performed")
	}
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
	tlsInfo := verifiedPeer.AuthInfo.(credentials.TLSInfo)
	leaf := tlsInfo.State.PeerCertificates[0]
	issuers := append([]*x509.Certificate{}, tlsInfo.State.PeerCertificates[1:]...)
	storedChain, _ := certs.ParseCertsFromPEM([]byte(auth.Certificates[0].PemCertificateChain))
	issuers = append(issuers, storedChain...)
	issuer, err := unenrollproof.Issuer(leaf, issuers)
	if err != nil {
		return err
	}
	j.Serial = leaf.SerialNumber.Text(16)
	svc := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn)
	ops := v2UnenrollOps{
		lookup: func(ctx context.Context, id string) (*v2AssetState, error) {
			tenant, device := principal.TenantUUID, principal.EntityID
			// Always use the binding selector: older UUID-only GetAsset servers
			// reject empty id before any revocation/deletion, even with --asset-id.
			// The selected UUID is still enforced by v2UnenrollLifecycle.
			asset, err := assets.GetAsset(rpcctx, &cloudpbv2.GetAssetRequest{OrganizationId: &tenant, DeviceId: &device})
			return v2AssetLookupResult(asset, err)
		},
		proof: func(ctx context.Context) ([]byte, error) { return unenrollproof.FetchRevocation(ctx, leaf, issuer) },
		delete: func(ctx context.Context, id string) error {
			reply := &cloudpbv2.DeleteAssetResponse{}
			expected := principal.EntityID
			err := cloudrequest.Invoke(rpcctx, cloudConn, auth, cloudpbv2.AssetService_DeleteAsset_FullMethodName, &cloudpbv2.DeleteAssetRequest{Id: id, ExpectedDeviceId: &expected}, reply)
			if err == nil && !reply.GetSuccess() {
				return fmt.Errorf("Cloud deletion was not acknowledged")
			}
			return err
		},
		reset: func(ctx context.Context, p, fp string, evidence []byte, deleted *cloudpbv2.DeletedAsset) error {
			binding, err := proto.Marshal(deleted)
			if err != nil {
				return err
			}
			response, err := svc.Unprovision(ctx, &agentpbv2.UnprovisionRequest{ExpectedPrincipalUri: p, ExpectedCertificateSha256: fp, RevocationProof: evidence, CloudDeletion: binding})
			if err != nil {
				return err
			}
			receipt, _, err := unenrollproof.ReadCompletion(response.GetUnenrollmentCompletion())
			if err != nil {
				return err
			}
			if receipt.Principal != p || receipt.Fingerprint != fp || receipt.AssetID != j.AssetID || receipt.Cloud != cloud || !bytes.Equal(receipt.CloudDeletion, binding) {
				return fmt.Errorf("local completion evidence mismatch")
			}
			return nil
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
		if jsonOutput {
			out, _ := json.Marshal(map[string]any{"principal": j.Principal, "cloudBindingVerified": true, "revocationAttempted": false, "assetDeleted": false, "deviceReset": false})
			fmt.Println(string(out))
		} else {
			fmt.Println("Cloud binding and guarded Agent capability verified. No revocation, deletion or reset attempted.")
		}
		return nil
	}
	// TODO(WDY-3585): Temporary intent confirmation while destructive CLI
	// step-up interactions are discussed. This is NOT fresh-factor approval
	// and cannot satisfy AAA or authorize normal-flow rollout.
	// https://linear.app/wendylabsinc/issue/WDY-3585/clarify-approval-interactions-for-destructive-cli-actions
	if !yes {
		if !isInteractiveTerminal() {
			return fmt.Errorf("unenroll is destructive; pass --yes to confirm")
		}
		fmt.Printf("Unenroll %s through Cloud asset %s at %s (PKI revocation and deletion), then reset this device.\n", j.Principal, j.AssetID, j.Cloud)
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
		fmt.Printf("Cloud unenrolled asset %s; authenticated local reset completed.\n", j.AssetID)
	}
	return nil
}
