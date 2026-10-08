package commands

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func runOIDCEnrollDevice(ctx context.Context, conn *grpcclient.AgentConnection, auth *config.AuthConfig, name string, orgOverride int32, directoryURL string) error {
	if orgOverride != 0 {
		return fmt.Errorf("--org is for legacy enrollment; OIDC enrollment uses the selected session's tenant")
	}
	if conn == nil || conn.Conn == nil {
		return fmt.Errorf("no device connection for ACME enrollment")
	}
	name, err := enrollmentDeviceName(conn, name)
	if err != nil {
		return err
	}
	// The identity is minted here and never derived from the name. device_id is
	// irreversible -- it becomes the SPIFFE SAN for the life of the device --
	// while the name is a renameable, organization-unique discovery key, so
	// deriving one from the other would make a relabelling rewrite an identity
	// that pki-core has already stamped into certificates.
	deviceID := uuid.NewString()
	cfg, err := cloudenroll.EnrollmentConfig(auth, deviceID, directoryURL)
	if err != nil {
		return err
	}
	if auth.CloudGRPC == "" {
		return fmt.Errorf("OIDC session has no Cloud gRPC endpoint")
	}

	if err := cloudenroll.CheckAgentEnrollment(ctx, conn.Conn); err != nil {
		return err
	}
	agent := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn)

	tokenCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer cloudConn.Close()

	fmt.Printf("Enrolling %s as %s with PKI...\n", name, deviceID)
	cfg, assetID, err := cloudenroll.MintEAB(tokenCtx, cloudConn, auth, cfg, name)
	if err != nil {
		return err
	}

	resp, err := agent.StartACMEProvisioning(ctx, &agentpbv2.StartACMEProvisioningRequest{
		CloudHost: auth.CloudGRPC, DirectoryUrl: cfg.DirectoryURL, DeviceId: cfg.DeviceID,
		EabKeyId: cfg.EABKeyID, EabHmacKey: cfg.EABHMACKey,
	})
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("Cloud registered asset %s, but this agent does not support direct PKI enrollment; update the agent (the device name is now reserved in Cloud)", assetID)
	}
	if err != nil {
		return fmt.Errorf("Cloud registered asset %s, but device PKI enrollment failed (the device name is now reserved in Cloud): %w", assetID, err)
	}
	expected, _ := cfg.PrincipalURI()
	if resp.GetPrincipalUri() != expected {
		return fmt.Errorf("agent returned an unexpected enrollment identity")
	}
	fmt.Printf("Device enrolled (name: %s, identity: %s, asset: %s).\n", name, resp.GetPrincipalUri(), assetID)
	return nil
}
