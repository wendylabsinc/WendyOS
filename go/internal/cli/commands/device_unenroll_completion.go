package commands

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

// Plaintext recovery transports public evidence only. Its server is not trusted
// as the old device: the old certificate's signature, configured trust roots and
// exact authorized Cloud tombstone authenticate that earlier operation.
func reconcileCompletedV2Unenroll(ctx context.Context, raw []byte, override, assetOverride string) error {
	return reconcileCompletedV2UnenrollPinned(ctx, raw, override, assetOverride, nil)
}

// The only fallback is a public read from the explicitly named/saved LAN host.
// It neither accepts that host's present identity nor changes its existing pin.
func recoverCompletedV2Unenroll(ctx context.Context, override, assetOverride string) error {
	addr, key, _, err := resolveDeviceAddress()
	if err != nil {
		return err
	}
	if key == "" || strings.Contains(key, ":") {
		return fmt.Errorf("completion recovery requires a named LAN target")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if port == "50052" {
		port = "50051"
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	var expected *config.DevicePin
	if pin, ok := cfg.DevicePinFor(key); ok {
		expected = &pin
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	conn, err := grpcclient.Connect(ctx, net.JoinHostPort(host, port))
	if err != nil {
		return err
	}
	defer conn.Close()
	reply, err := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if err != nil {
		return err
	}
	if reply.GetProvisioned() != nil || len(reply.GetNotProvisioned().GetUnenrollmentCompletion()) == 0 {
		return fmt.Errorf("no prior signed completion receipt; original identity refusal still applies")
	}
	return reconcileCompletedV2UnenrollPinned(ctx, reply.GetNotProvisioned().GetUnenrollmentCompletion(), override, assetOverride, expected)
}

func reconcileCompletedV2UnenrollPinned(ctx context.Context, raw []byte, override, assetOverride string, expected *config.DevicePin) error {
	receipt, leaf, err := unenrollproof.ReadCompletion(raw)
	if err != nil {
		return fmt.Errorf("authenticating prior reset completion: %w", err)
	}
	identity, err := certs.ParsePrincipal(receipt.Principal)
	if err != nil || identity.EntityType != certs.EntityAsset || (override != "" && override != receipt.Cloud) || (assetOverride != "" && assetOverride != receipt.AssetID) {
		return fmt.Errorf("completion identity/endpoint override mismatch")
	}
	if expected != nil && (expected.Principal == "" || expected.Principal != receipt.Principal || expected.CloudGRPC != receipt.Cloud) {
		return fmt.Errorf("completion evidence does not match the saved identity; pin retained")
	}
	auth, err := pickAuthEntry(receipt.Cloud)
	if err != nil {
		return err
	}
	auth, err = prepareEnrollmentAuth(ctx, auth)
	if err != nil {
		return err
	}
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != identity.TenantUUID {
		return fmt.Errorf("completion operator tenant mismatch")
	}
	roots, _ := certs.ParseCertsFromPEM([]byte(auth.Certificates[0].PemCertificateChain))
	peers, _ := certs.ParseCertsFromPEM([]byte(receipt.Chain))
	if receipt.AuthorizedAt <= 0 || time.Unix(receipt.AuthorizedAt, 0).After(time.Now()) {
		return fmt.Errorf("invalid completion time")
	}
	if err := certs.VerifyPeerCertificateChain(leaf, peers, roots, x509.ExtKeyUsageServerAuth, time.Unix(receipt.AuthorizedAt, 0), time.Unix(receipt.AuthorizedAt, 0)); err != nil {
		return fmt.Errorf("untrusted completion certificate: %w", err)
	}
	var retained cloudpbv2.DeletedAsset
	if err := proto.Unmarshal(receipt.CloudDeletion, &retained); err != nil {
		return err
	}
	j := v2UnenrollProgress{Cloud: receipt.Cloud, Principal: receipt.Principal, AssetID: receipt.AssetID, Fingerprint: receipt.Fingerprint, Serial: leaf.SerialNumber.Text(16)}
	if deleted, err := v2UnenrollLifecycle(&v2AssetState{Deleted: &retained}, &j); err != nil || !deleted {
		return fmt.Errorf("invalid signed completion binding")
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
	tenant, device := identity.TenantUUID, identity.EntityID
	active, lookupErr := cloudpbv2.NewAssetServiceClient(cloudConn).GetAsset(rpcctx, &cloudpbv2.GetAssetRequest{OrganizationId: &tenant, DeviceId: &device})
	state, err := v2AssetLookupResult(active, lookupErr)
	if err != nil {
		return fmt.Errorf("local reset evidence authentic, Cloud completion unconfirmed: %w", err)
	}
	deleted, err := v2UnenrollLifecycle(state, &j)
	if err != nil || !deleted || !proto.Equal(state.Deleted, &retained) {
		return fmt.Errorf("Cloud completion disagrees with signed reset evidence")
	}
	j.Revoked = true
	j.Deleted = true
	j.Reset = true
	if jsonOutput {
		data, _ := json.MarshalIndent(j, "", "  ")
		fmt.Println(string(data))
	} else {
		fmt.Printf("Confirmed prior Cloud unenrollment and local reset for %s. No mutation performed.\n", j.Principal)
	}
	return nil
}
