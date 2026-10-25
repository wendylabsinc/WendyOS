package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy-go/enrollment"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	grpcpeer "google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Output only: durable recovery evidence belongs to Cloud and Agent.
type v2UnenrollProgress = enrollment.Progress

func isLegacyCloudEnrollment(orgID, assetID int32) bool { return orgID > 0 && assetID > 0 }

type v2AssetState struct {
	Active  *cloudpbv2.Asset
	Deleted *cloudpbv2.DeletedAsset
}
type v2AuthorizedReset struct{ deletion, token []byte }
type v2UnenrollOps struct {
	lookup    func(context.Context, string) (*v2AssetState, error)
	authorize func(context.Context, string) (*v2AuthorizedReset, error)
	proof     func(context.Context) ([]byte, error)
	reset     func(context.Context, string, string, []byte, *v2AuthorizedReset) error
}

func sdkLifecycle(state *v2AssetState) (*enrollment.AssetState, error) {
	if state == nil {
		return nil, nil
	}
	out := new(enrollment.AssetState)
	if state.Active != nil {
		out.Active = new(enrollment.Asset)
		if err := copyEnrollmentMessage(state.Active, out.Active); err != nil {
			return nil, err
		}
	}
	if state.Deleted != nil {
		out.Deleted = new(enrollment.DeletedAsset)
		if err := copyEnrollmentMessage(state.Deleted, out.Deleted); err != nil {
			return nil, err
		}
	}
	return out, nil
}
func copyEnrollmentMessage(source, dest proto.Message) error {
	wire, err := proto.Marshal(source)
	if err != nil {
		return err
	}
	return proto.Unmarshal(wire, dest)
}
func sdkReset(a *v2AuthorizedReset) *enrollment.AuthorizedReset {
	if a == nil {
		return nil
	}
	return &enrollment.AuthorizedReset{Deletion: a.deletion, Token: a.token}
}
func v2UnenrollPreflight(state *v2AssetState, cause error, j *v2UnenrollProgress) error {
	shared, err := sdkLifecycle(state)
	if err != nil {
		return err
	}
	return enrollment.UnenrollPreflight(shared, cloudrequest.EnrollmentError(cause, nil, nil), j)
}
func v2AuthorizedDeletion(a *v2AuthorizedReset, j *v2UnenrollProgress) (*cloudpbv2.DeletedAsset, error) {
	shared, err := enrollment.AuthorizedDeletion(sdkReset(a), j)
	if err != nil {
		return nil, err
	}
	out := new(cloudpbv2.DeletedAsset)
	if err := copyEnrollmentMessage(shared, out); err != nil {
		return nil, err
	}
	return out, nil
}
func checkV2UnenrollBinding(asset *cloudpbv2.Asset, j v2UnenrollProgress) error {
	var shared *enrollment.Asset
	if asset != nil {
		shared = new(enrollment.Asset)
		if err := copyEnrollmentMessage(asset, shared); err != nil {
			return err
		}
	}
	return enrollment.CheckUnenrollBinding(shared, j)
}
func v2UnenrollLifecycle(state *v2AssetState, j *v2UnenrollProgress) (bool, error) {
	shared, err := sdkLifecycle(state)
	if err != nil {
		return false, err
	}
	return enrollment.UnenrollLifecycle(shared, j)
}
func performV2Unenroll(ctx context.Context, j *v2UnenrollProgress, ops v2UnenrollOps) error {
	return enrollment.PerformUnenroll(ctx, j, enrollment.UnenrollOps{
		Lookup: func(ctx context.Context, id string) (*enrollment.AssetState, error) {
			state, err := ops.lookup(ctx, id)
			if err != nil {
				return nil, cloudrequest.EnrollmentError(err, nil, nil)
			}
			return sdkLifecycle(state)
		},
		Authorize: func(ctx context.Context, id string) (*enrollment.AuthorizedReset, error) {
			a, err := ops.authorize(ctx, id)
			return sdkReset(a), err
		},
		Proof: ops.proof,
		Reset: func(ctx context.Context, principal, fingerprint string, proof []byte, a *enrollment.AuthorizedReset) error {
			var authorization *v2AuthorizedReset
			if a != nil {
				authorization = &v2AuthorizedReset{deletion: a.Deletion, token: a.Token}
			}
			return ops.reset(ctx, principal, fingerprint, proof, authorization)
		},
	})
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

// requireDeviceOperatorLeaf refuses before any Cloud mutation when the Cloud
// operator leaf is not the one the device connection presented: the Agent only
// erases on a reset signed by its mTLS peer, so a renewal or another session
// picked after the dial would let Cloud delete and revoke while the Agent refuses.
func requireDeviceOperatorLeaf(conn *grpcclient.AgentConnection, auth *config.AuthConfig) error {
	if conn.CertInfo == nil || conn.CertInfo.PemCertificate != auth.Certificates[0].PemCertificate {
		return fmt.Errorf("operator certificate changed since the device connection (renewed, or another session selected); rerun to reconnect with it; no mutation performed")
	}
	return nil
}

// Keep one flow at the affected Cloud boundary. cloudContext intentionally
// replaces authentication metadata; only the validated correlation crosses it.
func v2UnenrollCloudContext(ctx context.Context, auth *config.AuthConfig) (context.Context, error) {
	id, err := cloudrequest.CorrelationID(ctx)
	if err != nil {
		return nil, err
	}
	rpcctx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	return metadata.AppendToOutgoingContext(rpcctx, "x-correlation-id", id), nil
}

func runV2DeviceUnenroll(ctx context.Context, conn *grpcclient.AgentConnection, prov *agentpbv2.ProvisionedResponse, verifiedPeer *grpcpeer.Peer, override, assetOverride string, yes, checkOnly, force bool) error {
	// SECURITY: only the authenticated-reset bit counts. CloudUnenrollmentSupported
	// was advertised by prerelease Agents that trusted caller-supplied deletion
	// fields, so it is ignored here, including for --check. --force is a separate
	// local-only mode with its own capability; it never runs the Cloud flow.
	if force {
		if assetOverride != "" {
			return fmt.Errorf("--force clears only a local enrollment and cannot be combined with --asset-id")
		}
		if !prov.GetLocalResetSupported() {
			return fmt.Errorf("this Agent does not support forced local enrollment reset; update it with 'wendy device update' and retry; no mutation performed")
		}
	} else if !prov.GetCloudAuthenticatedResetSupported() {
		return fmt.Errorf("this Agent does not support Cloud-authenticated reset; update it with 'wendy device update' and retry; no mutation performed")
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
	if err := requireDeviceOperatorLeaf(conn, auth); err != nil {
		return err
	}
	rpcctx, err := v2UnenrollCloudContext(ctx, auth)
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
	tlsInfo, ok := verifiedPeer.AuthInfo.(credentials.TLSInfo)
	if !ok {
		return fmt.Errorf("verified peer certificate unavailable; no mutation performed")
	}
	leaf := tlsInfo.State.PeerCertificates[0]
	operator, err := certs.ParseCertsFromPEM([]byte(auth.Certificates[0].PemCertificate))
	if err != nil || len(operator) != 1 {
		return fmt.Errorf("exact reset operator certificate required; no mutation performed")
	}
	j.Serial = leaf.SerialNumber.Text(16)
	var issuer *x509.Certificate
	svc := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn)
	ops := v2UnenrollOps{
		lookup: func(ctx context.Context, id string) (*v2AssetState, error) {
			tenant, device := principal.TenantUUID, principal.EntityID
			asset, err := assets.GetAsset(rpcctx, &cloudpbv2.GetAssetRequest{Id: id, OrganizationId: &tenant, DeviceId: &device})
			if err != nil {
				return nil, err
			}
			return &v2AssetState{Active: asset}, nil
		},
		authorize: func(ctx context.Context, id string) (*v2AuthorizedReset, error) {
			expected := principal.EntityID
			reply := &cloudpbv2.DeleteAssetResponse{}
			if err := cloudrequest.Invoke(rpcctx, cloudConn, auth, cloudpbv2.AssetService_DeleteAsset_FullMethodName,
				&cloudpbv2.DeleteAssetRequest{Id: id, ExpectedDeviceId: &expected, ResetCertificateSha256: &fingerprint}, reply); err != nil {
				return nil, err
			}
			if !reply.GetSuccess() || len(reply.GetResetAuthorization()) == 0 {
				return nil, fmt.Errorf("updated Cloud with signed reset authorization required; keys retained")
			}
			now := time.Now()
			deletion, err := unenrollproof.VerifyCloudResetDeletion(ctx, reply.GetResetAuthorization(), cloud, principal.Principal, fingerprint, operator[0], now, now)
			if err != nil {
				return nil, fmt.Errorf("authenticating Cloud reset response: %w", err)
			}
			return &v2AuthorizedReset{deletion: deletion, token: reply.GetResetAuthorization()}, nil
		},
		proof: func(ctx context.Context) ([]byte, error) { return unenrollproof.FetchRevocation(ctx, leaf, issuer) },
		reset: func(ctx context.Context, p, fp string, evidence []byte, authorized *v2AuthorizedReset) error {
			binding := authorized.deletion
			response, err := svc.Unprovision(ctx, &agentpbv2.UnprovisionRequest{ExpectedPrincipalUri: p, ExpectedCertificateSha256: fp, RevocationProof: evidence, CloudDeletion: binding, CloudResetAuthorization: authorized.token})
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
	if force {
		// Cloud also answers NotFound to a caller who cannot see the device, so
		// NotFound alone never selects local reset: the operator must ask for it.
		// A device Cloud still lists keeps the Cloud flow.
		switch {
		case err == nil:
			return fmt.Errorf("Cloud still lists %s; --force only clears a device Cloud has no record of; rerun without --force to unenroll it through Cloud; no mutation performed", principal.Principal)
		case status.Code(err) != codes.NotFound:
			return fmt.Errorf("checking Cloud binding before forced local reset: %w", err)
		}
		return runV2LocalDeviceUnenroll(ctx, conn, prov, verifiedPeer, yes, checkOnly)
	}
	if checkOnly && err != nil {
		return fmt.Errorf("checking active Cloud binding: %w", err)
	}
	if err := v2UnenrollPreflight(reply, err, &j); err != nil {
		return fmt.Errorf("reading Cloud lifecycle: %w", err)
	}
	// Cloud cleanup still requires the installed leaf's issuer before mutation.
	// Forced local cleanup needs neither an issuer nor OCSP evidence.
	issuers := append([]*x509.Certificate{}, tlsInfo.State.PeerCertificates[1:]...)
	storedChain, _ := certs.ParseCertsFromPEM([]byte(auth.Certificates[0].PemCertificateChain))
	issuers = append(issuers, storedChain...)
	issuer, err = unenrollproof.Issuer(leaf, issuers)
	if err != nil {
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
	// SECURITY: AAA v0.22 (retained in v0.24), §5.1/§7(4), excludes removal-only
	// unenrollment from step-up. This prompt confirms intent, not identity;
	// exact device:delete authorization and signed-body checks remain required.
	// Amendment: https://linear.app/wendylabsinc/issue/WDY-3585/clarify-approval-interactions-for-destructive-cli-actions
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
