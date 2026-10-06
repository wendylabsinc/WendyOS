package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Nonsecret transaction state survives an uncertain response or failed reset.
// A missing Cloud row is never permission to erase keys without this previously
// authenticated mapping and the Agent's durable installed-cert revocation ack.
type v2UnenrollJournal struct {
	Cloud       string `json:"cloud"`
	Principal   string `json:"principal"`
	AssetID     string `json:"assetId"`
	Fingerprint string `json:"certificateSHA256,omitempty"`
	Serial      string `json:"certificateSerial,omitempty"`
	Revoked     bool   `json:"certificateRevoked"`
	Deleted     bool   `json:"assetDeleted"`
	Reset       bool   `json:"deviceReset"`
}

type v2UnenrollOps struct {
	get    func(context.Context, string) (*cloudpbv2.Asset, error)
	revoke func(context.Context, string, string) (*agentpbv2.RevokeACMECertificateResponse, error)
	delete func(context.Context, string) error
	reset  func(context.Context, string, string) error
	save   func(v2UnenrollJournal) error
}

func checkV2UnenrollBinding(asset *cloudpbv2.Asset, j v2UnenrollJournal) error {
	id, err := certs.ParsePrincipal(j.Principal)
	if err != nil || id.EntityType != certs.EntityAsset || asset == nil || asset.GetId() != j.AssetID || asset.GetOrganizationId() != id.TenantUUID || asset.GetPkiDeviceName() != id.EntityID {
		return fmt.Errorf("refusing cleanup: exact Cloud asset/tenant/PKI binding does not match the authenticated device")
	}
	return nil
}

func performV2Unenroll(ctx context.Context, j *v2UnenrollJournal, ops v2UnenrollOps) error {
	if len(j.Fingerprint) != 64 {
		return fmt.Errorf("verified installed certificate fingerprint required")
	}
	if _, err := hex.DecodeString(j.Fingerprint); err != nil {
		return fmt.Errorf("invalid installed certificate fingerprint")
	}
	// Always re-read mapping, even on retry; do not delete a rebound asset.
	asset, err := ops.get(ctx, j.AssetID)
	missing := status.Code(err) == codes.NotFound
	if err != nil && !(missing && j.Revoked) {
		return fmt.Errorf("preflight Cloud asset mapping: %w", err)
	}
	if !missing {
		if err := checkV2UnenrollBinding(asset, *j); err != nil {
			return err
		}
	}
	if err := ops.save(*j); err != nil {
		return fmt.Errorf("saving cleanup identity before revocation: %w", err)
	}
	// RPC is idempotent through the Agent's durable fingerprint-specific ack.
	ack, err := ops.revoke(ctx, j.Principal, j.Fingerprint)
	if err != nil {
		return fmt.Errorf("revoking installed ACME certificate; no keys reset: %w", err)
	}
	if ack.GetPrincipalUri() != j.Principal || len(ack.GetCertificateSha256()) != 64 || ack.GetCertificateSerial() == "" {
		return fmt.Errorf("invalid revocation acknowledgement; no asset deletion or key reset")
	}
	if _, err := hex.DecodeString(ack.GetCertificateSha256()); err != nil {
		return fmt.Errorf("invalid certificate fingerprint")
	}
	if j.Fingerprint != ack.GetCertificateSha256() {
		return fmt.Errorf("installed certificate changed during cleanup; refusing deletion/reset")
	}
	j.Revoked = true
	j.Fingerprint = ack.GetCertificateSha256()
	j.Serial = ack.GetCertificateSerial()
	if err := ops.save(*j); err != nil {
		return fmt.Errorf("revoked certificate but progress could not be saved; keys retained: %w", err)
	}
	if !missing {
		// Revocation may take time; recheck the authoritative identity immediately
		// before signed deletion. This is convergent, not a distributed transaction.
		current, err := ops.get(ctx, j.AssetID)
		if err != nil {
			return fmt.Errorf("rechecking Cloud binding after revocation; keys retained: %w", err)
		}
		if err := checkV2UnenrollBinding(current, *j); err != nil {
			return err
		}
		if err := ops.delete(ctx, j.AssetID); err != nil && status.Code(err) != codes.NotFound {
			return fmt.Errorf("certificate revoked; Cloud deletion uncertain/failed; keys retained: %w", err)
		}
	}
	j.Deleted = true
	if err := ops.save(*j); err != nil {
		return fmt.Errorf("Cloud asset deleted; reset deferred until progress can be saved: %w", err)
	}
	if err := ops.reset(ctx, j.Principal, j.Fingerprint); err != nil {
		return fmt.Errorf("certificate revoked and asset deleted; local reset unconfirmed; reconcile/retry this transaction: %w", err)
	}
	j.Reset = true
	return ops.save(*j)
}

func directUnenrollPeerFingerprint(conn *grpcclient.AgentConnection, verifiedPeer *grpcpeer.Peer, principal certs.WendyIdentity) (string, error) {
	// The legacy ObservedServerIdentity sink drops numeric org 0, including
	// valid UUID tenant identities. Use the authenticated direct RPC transport's
	// peer certificate below, not that numeric-only bookkeeping cache.
	if conn == nil || !conn.IsMTLS || conn.IsSessionProxy || verifiedPeer == nil {
		return "", fmt.Errorf("v2 unenroll requires a directly verified mTLS device connection; use its LAN hostname/IP")
	}
	peerTLS, ok := verifiedPeer.AuthInfo.(credentials.TLSInfo)
	if !ok || !peerTLS.State.HandshakeComplete || len(peerTLS.State.PeerCertificates) == 0 {
		return "", fmt.Errorf("verified peer certificate unavailable; no mutation performed")
	}
	leaf := peerTLS.State.PeerCertificates[0]
	peerPrincipal, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok || peerPrincipal != principal.Principal {
		return "", fmt.Errorf("Agent state disagrees with its verified TLS identity")
	}
	peerDigest := sha256.Sum256(leaf.Raw)
	return hex.EncodeToString(peerDigest[:]), nil
}

func runV2DeviceUnenroll(ctx context.Context, conn *grpcclient.AgentConnection, prov *agentpbv2.ProvisionedResponse, verifiedPeer *grpcpeer.Peer, override, assetOverride string, yes, checkOnly bool) error {
	principal, err := certs.ParsePrincipal(prov.GetPrincipalUri())
	if err != nil || principal.EntityType != certs.EntityAsset {
		return fmt.Errorf("invalid direct PKI device identity")
	}
	peerFingerprint, err := directUnenrollPeerFingerprint(conn, verifiedPeer, principal)
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
	dir, err := config.ConfigDir()
	if err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(cloud + "\x00" + principal.Principal))
	journalPath := filepath.Join(dir, "unenroll-v2", hex.EncodeToString(digest[:])+".json")
	j := v2UnenrollJournal{Cloud: cloud, Principal: principal.Principal}
	data, readErr := os.ReadFile(journalPath)
	if readErr == nil {
		if json.Unmarshal(data, &j) != nil || j.Cloud != cloud || j.Principal != principal.Principal || j.AssetID == "" {
			return fmt.Errorf("cleanup journal is invalid; no mutation performed")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("reading cleanup progress failed")
	}
	if j.Reset {
		j = v2UnenrollJournal{Cloud: cloud, Principal: principal.Principal}
	}
	if j.Fingerprint != "" && j.Fingerprint != peerFingerprint {
		return fmt.Errorf("installed certificate differs from the unfinished cleanup transaction; no mutation performed")
	}
	j.Fingerprint = peerFingerprint
	if assetOverride != "" {
		id, err := uuid.Parse(assetOverride)
		if err != nil || id.String() != assetOverride {
			return fmt.Errorf("--asset-id requires a canonical UUID")
		}
		if j.AssetID != "" && j.AssetID != assetOverride {
			return fmt.Errorf("requested asset differs from the recorded cleanup transaction")
		}
		j.AssetID = assetOverride
	}
	if j.AssetID == "" {
		candidates, err := fetchCloudAssetsV2(ctx, auth, false)
		if err != nil {
			return err
		}
		for _, a := range candidates {
			if a.GetOrganizationId() == principal.TenantUUID && a.GetPkiDeviceName() == principal.EntityID {
				if j.AssetID != "" {
					return fmt.Errorf("multiple Cloud assets bind this device; use an exact verified --asset-id")
				}
				j.AssetID = a.GetId()
			}
		}
		if j.AssetID == "" {
			return fmt.Errorf("no authoritative Cloud binding for this device; no mutation performed")
		}
	}
	id, err := uuid.Parse(j.AssetID)
	if err != nil || id.String() != j.AssetID {
		return fmt.Errorf("invalid Cloud asset UUID")
	}
	// Read preflight before the confirmation; never infer identity from the UUID.
	asset, err := assets.GetAsset(rpcctx, &cloudpbv2.GetAssetRequest{Id: j.AssetID})
	if err != nil && !(status.Code(err) == codes.NotFound && j.Revoked) {
		return fmt.Errorf("reading exact Cloud binding: %w", err)
	}
	if err == nil {
		if err := checkV2UnenrollBinding(asset, j); err != nil {
			return err
		}
	}
	if checkOnly {
		_, probeErr := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn).CheckACMERevocation(ctx, &agentpbv2.RevokeACMECertificateRequest{ExpectedPrincipalUri: j.Principal, ExpectedCertificateSha256: j.Fingerprint})
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
	svc := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn)
	ops := v2UnenrollOps{
		get: func(ctx context.Context, id string) (*cloudpbv2.Asset, error) {
			return assets.GetAsset(rpcctx, &cloudpbv2.GetAssetRequest{Id: id})
		},
		revoke: func(ctx context.Context, p, fingerprint string) (*agentpbv2.RevokeACMECertificateResponse, error) {
			return svc.RevokeACMECertificate(ctx, &agentpbv2.RevokeACMECertificateRequest{ExpectedPrincipalUri: p, ExpectedCertificateSha256: fingerprint})
		},
		delete: func(ctx context.Context, id string) error {
			return cloudrequest.Invoke(rpcctx, cloudConn, auth,
				cloudpbv2.AssetService_DeleteAsset_FullMethodName,
				&cloudpbv2.DeleteAssetRequest{Id: id}, &cloudpbv2.DeleteAssetResponse{})
		},
		reset: func(ctx context.Context, p, fingerprint string) error {
			_, err := svc.Unprovision(ctx, &agentpbv2.UnprovisionRequest{ExpectedPrincipalUri: p, ExpectedCertificateSha256: fingerprint})
			return err
		},
		save: func(j v2UnenrollJournal) error { return saveV2UnenrollJournal(journalPath, j) },
	}
	err = performV2Unenroll(ctx, &j, ops)
	if jsonOutput {
		out, _ := json.MarshalIndent(j, "", "  ")
		fmt.Println(string(out))
	}
	if err != nil {
		return err
	}
	// Clear ONLY this deliberately reset principal, including its host aliases.
	if err := config.Update(func(cfg *config.Config) (bool, error) { clearPinsForIdentity(cfg, principal); return true, nil }); err != nil {
		return fmt.Errorf("unenrolled; local identity pin cleanup failed: %w", err)
	}
	if !jsonOutput {
		fmt.Printf("Revoked installed certificate, deleted asset %s, and reset the device.\n", j.AssetID)
	}
	return nil
}

func saveV2UnenrollJournal(path string, j v2UnenrollJournal) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".unenroll-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	defer os.Remove(tmp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
