package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
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
	deviceID, err := cloudenroll.NewDeviceID(ctx)
	if err != nil {
		return err
	}
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
	hw, err := cloudenroll.ReadHardware(ctx, conn.Conn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v; enrolling without hardware details.\n", err)
	}

	tokenCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer cloudConn.Close()

	fmt.Printf("Enrolling %s with PKI...\n", name)
	cfg, assetID, err := cloudenroll.MintEAB(tokenCtx, cloudConn, auth, cfg, name, hw)
	if err != nil {
		return err
	}

	principal, err := cloudenroll.StartACMEProvisioning(ctx, conn.Conn, auth.CloudGRPC, cfg, assetID)
	if err != nil {
		return err
	}
	fmt.Printf("Device enrolled (name: %s, identity: %s, asset: %s).\n", name, principal, assetID)
	return nil
}
