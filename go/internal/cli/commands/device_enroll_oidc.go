package commands

import (
	"context"
	"fmt"
	"os"

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

	agent := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn)
	provisioned, err := agent.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("this agent does not support direct PKI enrollment; update the agent and retry")
	}
	if err != nil {
		return fmt.Errorf("checking device enrollment: %w", err)
	}
	if provisioned.GetProvisioned() != nil {
		return fmt.Errorf("device is already enrolled; unprovision it before enrolling again")
	}
	if provisioned.GetNotProvisioned() == nil {
		return fmt.Errorf("agent returned an unknown enrollment state")
	}

	pending, resumed, err := cloudenroll.LoadPending(cfg, name)
	if err != nil {
		return err
	}
	if resumed {
		cfg = pending
		fmt.Printf("Resuming enrollment of %s as device %s with the credential Cloud issued earlier...\n", name, cfg.DeviceID)
	} else {
		tokenCtx, err := cloudContext(ctx, auth)
		if err != nil {
			return err
		}
		cloudConn, err := dialCloudGRPC(auth)
		if err != nil {
			return err
		}
		defer cloudConn.Close()

		fmt.Printf("Enrolling %s as device %s with PKI...\n", name, deviceID)
		if cfg, _, err = cloudenroll.MintEAB(tokenCtx, cloudConn, auth, cfg, name); err != nil {
			return err
		}
		if err := cloudenroll.SavePending(cfg, name); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not keep the enrollment credential (%v); if the next step fails, re-running cannot resume it.\n", err)
		}
	}

	resp, err := agent.StartACMEProvisioning(ctx, &agentpbv2.StartACMEProvisioningRequest{
		CloudHost: auth.CloudGRPC, DirectoryUrl: cfg.DirectoryURL, DeviceId: cfg.DeviceID,
		EabKeyId: cfg.EABKeyID, EabHmacKey: cfg.EABHMACKey,
	})
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("Cloud reserved %s for device %s, but this agent does not support direct PKI enrollment; update the agent, then re-run this command to finish", name, cfg.DeviceID)
	}
	if err != nil {
		path, _ := cloudenroll.PendingPath(cfg, name)
		return fmt.Errorf("Cloud reserved %s for device %s, but device PKI enrollment failed: %w\nFix the cause above, then re-run this command: it resumes with the same device and the credential kept in %s", name, cfg.DeviceID, err, path)
	}
	if err := cloudenroll.ClearPending(cfg, name); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not remove the redeemed enrollment credential: %v\n", err)
	}
	expected, _ := cfg.PrincipalURI()
	if resp.GetPrincipalUri() != expected {
		return fmt.Errorf("agent returned an unexpected enrollment identity")
	}
	fmt.Printf("Device enrolled (name: %s, device id: %s, identity: %s).\n", name, cfg.DeviceID, resp.GetPrincipalUri())
	return nil
}
