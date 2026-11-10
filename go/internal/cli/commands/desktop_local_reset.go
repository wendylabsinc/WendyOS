package commands

import (
	"context"
	"errors"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/peer"
)

// DesktopLocalResetPlan is bound to the installed leaf, not merely a hostname.
type DesktopLocalResetPlan struct{ Principal, Fingerprint string }

func DesktopPrepareLocalReset(ctx context.Context, conn *grpcclient.AgentConnection, account, organization, device string) (DesktopLocalResetPlan, error) {
	if conn == nil || conn.Conn == nil || !conn.IsMTLS || conn.IsSessionProxy {
		return DesktopLocalResetPlan{}, errors.New("connect directly to this device on your local network to reset its enrollment")
	}
	var remote peer.Peer
	state, err := pb.NewWendyProvisioningServiceClient(conn.Conn).IsProvisioned(ctx, &pb.IsProvisionedRequest{}, grpc.Peer(&remote))
	if err != nil {
		return DesktopLocalResetPlan{}, err
	}
	return desktopLocalResetPlan(conn, state.GetProvisioned(), &remote, account, organization, device)
}

func desktopLocalResetPlan(conn *grpcclient.AgentConnection, prov *pb.ProvisionedResponse, remote *peer.Peer, account, organization, device string) (DesktopLocalResetPlan, error) {
	var plan DesktopLocalResetPlan
	if account == "" || organization == "" || device == "" {
		return plan, errors.New("choose this device's signed-in account first")
	}
	if !prov.GetLocalResetSupported() {
		return plan, errors.New("this Agent does not support local enrollment reset; update the Agent first")
	}
	identity, err := certs.ParsePrincipal(prov.GetPrincipalUri())
	if err != nil || identity.EntityType != certs.EntityAsset || identity.TenantUUID != organization || identity.EntityID != device {
		return plan, errors.New("device enrollment changed; refresh its identity")
	}
	fingerprint, err := directUnenrollPeerFingerprint(conn, remote, identity)
	if err != nil {
		return plan, err
	}
	auth, err := desktopCloudAuth(account, organization)
	if err != nil {
		return plan, err
	}
	if auth.CloudGRPC == "" || auth.CloudGRPC != prov.GetCloudHost() {
		return plan, errors.New("account and device belong to different Cloud servers")
	}
	if err := requireDeviceOperatorLeaf(conn, auth); err != nil {
		return plan, err
	}
	operator, err := certs.ParsePrincipal(conn.CertInfo.CertificatePrincipal())
	if err != nil || operator.EntityType != certs.EntityUser || operator.TenantUUID != identity.TenantUUID {
		return plan, errors.New("local reset requires an operator from this organization")
	}
	return DesktopLocalResetPlan{identity.Principal, fingerprint}, nil
}

// DesktopResetLocalEnrollment rechecks the confirmed leaf immediately before the
// one-shot mutation. A dropped reply is unconfirmed; it is never retried here.
func DesktopResetLocalEnrollment(ctx context.Context, conn *grpcclient.AgentConnection, account, organization, device string, expected DesktopLocalResetPlan) (bool, error) {
	current, err := DesktopPrepareLocalReset(ctx, conn, account, organization, device)
	if err != nil {
		return false, err
	}
	if expected.Principal == "" || expected.Fingerprint == "" || current != expected {
		return false, errors.New("device identity changed since confirmation; no reset was sent")
	}
	return desktopApplyLocalReset(ctx, conn, current)
}

func desktopApplyLocalReset(ctx context.Context, conn *grpcclient.AgentConnection, plan DesktopLocalResetPlan) (bool, error) {
	reply, err := pb.NewWendyProvisioningServiceClient(conn.Conn).Unprovision(ctx, &pb.UnprovisionRequest{LocalOnly: true, ExpectedPrincipalUri: plan.Principal, ExpectedCertificateSha256: plan.Fingerprint})
	if err != nil {
		return false, fmt.Errorf("reset was not confirmed; the device may already be unenrolled. Check nearby setup before trying again: %w", err)
	}
	if !reply.GetLocalResetCompleted() {
		return false, errors.New("the Agent did not confirm completion; check nearby setup before trying again")
	}
	identity, err := certs.ParsePrincipal(plan.Principal)
	if err != nil {
		return true, fmt.Errorf("reset completed, but local identity cleanup failed: %w", err)
	}
	if err := config.Update(func(cfg *config.Config) (bool, error) { clearPinsForIdentity(cfg, identity); return true, nil }); err != nil {
		return true, fmt.Errorf("reset completed, but this app's identity pins could not be cleared: %w", err)
	}
	return true, nil
}
