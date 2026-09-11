package commands

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Resolve and validate the destination before Cloud mints a once-only credential.
func oidcEnrollmentConfig(auth *config.AuthConfig, name, directoryURL string) (acmeenroll.Config, error) {
	cfg := acmeenroll.Config{DeviceID: name, DirectoryURL: directoryURL}
	u, err := url.Parse(auth.Certificates[0].PrincipalURI)
	if err != nil || u.Scheme != "spiffe" || u.Host != "wendy.sh" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return cfg, fmt.Errorf("OIDC session has no valid operator identity; re-run 'wendy auth login'")
	}
	parts := strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/")
	if len(parts) != 4 || parts[0] != "tenant" || parts[2] != "operator" || parts[3] == "" {
		return cfg, fmt.Errorf("OIDC session must carry a tenant operator identity")
	}
	tenant, err := uuid.Parse(parts[1])
	if err != nil || tenant.String() != parts[1] {
		return cfg, fmt.Errorf("OIDC session has an invalid tenant UUID")
	}
	if cfg.DirectoryURL == "" {
		identityURL, _ := url.Parse(auth.PKIEndpoint)
		if identityURL != nil && identityURL.Scheme == "https" && identityURL.Host == "identity.dev.pki.wendy.sh" {
			cfg.DirectoryURL = "https://acme.dev.pki.wendy.sh/" + tenant.String() + "/acme/directory"
		} else {
			return cfg, fmt.Errorf("no ACME directory configured for this PKI deployment; pass --acme-directory-url")
		}
	}
	principal, err := cfg.PrincipalURI()
	if err != nil {
		return cfg, err
	}
	if principal != "spiffe://wendy.sh/tenant/"+tenant.String()+"/device/"+name {
		return cfg, fmt.Errorf("ACME enrollment tenant does not match the selected OIDC session")
	}
	return cfg, nil
}

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
	cfg, err := oidcEnrollmentConfig(auth, name, directoryURL)
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

	tokenCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	artifact, err := cloudrequest.EnrollmentRequest(auth, name)
	if err != nil {
		return fmt.Errorf("signing enrollment request: %w", err)
	}
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer cloudConn.Close()

	fmt.Printf("Enrolling %s with PKI...\n", name)
	credential, err := cloudpbv2.NewDeviceEnrollmentServiceClient(cloudConn).EnrollDevice(tokenCtx, &cloudpbv2.EnrollDeviceRequest{
		DeviceId: name, DeviceClass: cloudpbv2.DeviceClass_DEVICE_CLASS_B,
		EnrollmentRequestJws: artifact, Name: name,
	})
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("this Cloud deployment does not support OIDC device enrollment; it needs DeviceEnrollmentService/EnrollDevice")
	}
	if err != nil {
		return fmt.Errorf("creating PKI enrollment credential: %w", err)
	}
	if credential.GetCredentialKind() != "eab" {
		return fmt.Errorf("Cloud returned an unexpected enrollment credential kind")
	}
	cfg.EABKeyID, cfg.EABHMACKey = credential.GetEabKeyId(), credential.GetEabHmacKey()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("Cloud returned invalid enrollment credentials: %w", err)
	}

	resp, err := agent.StartACMEProvisioning(ctx, &agentpbv2.StartACMEProvisioningRequest{
		CloudHost: auth.CloudGRPC, DirectoryUrl: cfg.DirectoryURL, DeviceId: cfg.DeviceID,
		EabKeyId: cfg.EABKeyID, EabHmacKey: cfg.EABHMACKey,
	})
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("Cloud registered asset %s, but this agent does not support direct PKI enrollment; update the agent (the device name is now reserved in Cloud)", credential.GetAssetId())
	}
	if err != nil {
		return fmt.Errorf("Cloud registered asset %s, but device PKI enrollment failed (the device name is now reserved in Cloud): %w", credential.GetAssetId(), err)
	}
	expected, _ := cfg.PrincipalURI()
	if resp.GetPrincipalUri() != expected {
		return fmt.Errorf("agent returned an unexpected enrollment identity")
	}
	fmt.Printf("Device enrolled (%s, asset: %s).\n", resp.GetPrincipalUri(), credential.GetAssetId())
	return nil
}
