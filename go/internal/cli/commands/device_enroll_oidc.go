package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func runOIDCEnrollDevice(ctx context.Context, conn *grpcclient.AgentConnection, auth *config.AuthConfig, name string, orgOverride int32, directoryURL string) error {
	_, err := runOIDCEnrollDeviceIdentity(ctx, conn, auth, name, orgOverride, directoryURL)
	return err
}

func runOIDCEnrollDeviceIdentity(ctx context.Context, conn *grpcclient.AgentConnection, auth *config.AuthConfig, name string, orgOverride int32, directoryURL string) (*certs.WendyIdentity, error) {
	if orgOverride != 0 {
		return nil, fmt.Errorf("--org is for legacy enrollment; OIDC enrollment uses the selected session's tenant")
	}
	if conn == nil || conn.Conn == nil {
		return nil, fmt.Errorf("no device connection for ACME enrollment")
	}
	name, err := enrollmentDeviceName(conn, name)
	if err != nil {
		return nil, err
	}
	// The identity is minted here and never derived from the name. device_id is
	// irreversible -- it becomes the SPIFFE SAN for the life of the device --
	// while the name is a renameable, organization-unique discovery key, so
	// deriving one from the other would make a relabelling rewrite an identity
	// that pki-core has already stamped into certificates.
	deviceID := uuid.NewString()
	cfg, err := cloudenroll.EnrollmentConfig(auth, deviceID, directoryURL)
	if err != nil {
		return nil, err
	}
	if auth.CloudGRPC == "" {
		return nil, fmt.Errorf("OIDC session has no Cloud gRPC endpoint")
	}

	if err := cloudenroll.CheckAgentEnrollment(ctx, conn.Conn); err != nil {
		return nil, err
	}
	agent := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn)
	hw, err := cloudenroll.ReadHardware(ctx, conn.Conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v; enrolling without hardware details.\n", err)
	}

	tokenCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer cloudConn.Close()

	fmt.Printf("Enrolling %s with PKI...\n", name)
	cfg, assetID, err := cloudenroll.MintEAB(tokenCtx, cloudConn, auth, cfg, name, hw)
	if err != nil {
		return nil, err
	}

	resp, err := agent.StartACMEProvisioning(ctx, &agentpbv2.StartACMEProvisioningRequest{
		CloudHost: auth.CloudGRPC, DirectoryUrl: cfg.DirectoryURL, DeviceId: cfg.DeviceID,
		EabKeyId: cfg.EABKeyID, EabHmacKey: cfg.EABHMACKey,
	})
	if status.Code(err) == codes.Unimplemented {
		return nil, fmt.Errorf("Cloud registered asset %s, but this agent does not support direct PKI enrollment; update the agent (the device name is now reserved in Cloud)", assetID)
	}
	if err != nil {
		return nil, fmt.Errorf("Cloud registered asset %s, but device PKI enrollment failed (the device name is now reserved in Cloud): %w", assetID, err)
	}
	expected, _ := cfg.PrincipalURI()
	if resp.GetPrincipalUri() != expected {
		return nil, fmt.Errorf("agent returned an unexpected enrollment identity")
	}
	fmt.Printf("Device enrolled (name: %s, identity: %s, asset: %s).\n", name, resp.GetPrincipalUri(), assetID)
	identity, err := certs.ParseDevicePrincipal(expected)
	return &identity, err
}
