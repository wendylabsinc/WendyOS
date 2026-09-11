package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/enrolltoken"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// defaultAgentConfigPath is the agent's config directory, the same default
// wendy-agent applies and overrides with WENDY_CONFIG_PATH.
const defaultAgentConfigPath = "/etc/wendy-agent"

// newDataEnrollCmd hands the agent a pki-core enrollment token.
//
// TWO PATHS, ONE FILE. The agent takes exactly one one-shot provisioning input
// this way: a credential file under its config directory, which it redeems and
// deletes. enrollment.json does it for the Wendy Cloud identity, and
// pki-enrollment.json does it for the pki-core one. The credential stays off
// the control plane and out of any long-lived configuration file, and it is
// deleted once spent.
//
// The default path is the RPC. A WendyOS device has no shell: nothing outside
// the device can write a file into /etc/wendy-agent, so the only way a token
// reaches a device already in the field is over the agent's own gRPC surface,
// which is what StagePKIEnrollment is for. It writes the same file, on the
// device, and applies it at once - so enrolment finishes while this command is
// still running instead of waiting for a restart.
//
// --local keeps the on-device path for the installer case, where this command
// runs next to the agent whose files it writes and there may not yet be an
// agent listening to call.
func newDataEnrollCmd() *cobra.Command {
	var (
		tenant      string
		token       string
		deviceID    string
		csrEndpoint string
		environment string
		configPath  string
		local       bool
	)
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Enroll this device's pki-core data platform identity",
		Long: strings.TrimSpace(`
Hand the agent a pki-core enrollment token so it enrolls a data platform device
identity.

The identity is a second certificate: its only URI Subject Alternative Name is
spiffe://wendy.sh/tenant/<tenant>/device/<name>, and it is presented to the
Wendy Data Platform ingest endpoint. The certificate the device presents to
Wendy Cloud is not touched, because Wendy Cloud's interceptors read only the
urn:wendy:org: form.

The token is minted by an operator through pki-core's fabric relay and is
single-use. --tenant may be omitted when the token carries a tenant_uuid claim.
The SPIFFE device name is fixed when the token is minted, so --device-id must
match the token's device_id byte for byte; omit it to use the certificate
Common Name the agent already builds.

By default the token is sent to the agent over gRPC, which stages it on the
device and enrolls immediately; the outcome is printed. With --local the token
is instead written to this machine's own agent config directory for the agent
to redeem on its next start, which is the installer's path.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(token) == "" {
				return errors.New("--token is required")
			}
			// Refuse a token the enrolment request could not carry before
			// anything is staged or dialled. The usual cause is a paste that
			// brought a heading or a trailing newline with it, and the failure
			// it produces downstream reads like a refusal from pki-core.
			if err := pkienroll.ValidateToken(token); err != nil {
				return fmt.Errorf("--token: %w", err)
			}
			// A --config-path is an unambiguous statement about a directory on
			// THIS machine, so it selects the local path on its own. Sending it
			// to a remote agent would be meaningless: the agent writes into its
			// own config directory and nothing else.
			if configPath != "" {
				local = true
			}
			if strings.TrimSpace(tenant) == "" {
				claimed, ok := enrolltoken.TenantUUIDFromToken(token)
				if !ok && local {
					// The remote path can still succeed here: the agent
					// resolves the tenant itself and answers with the reason
					// when it cannot. Only the local path, which never talks
					// to anything, has to refuse now.
					return errors.New("--tenant is required: the token carries no tenant_uuid claim")
				}
				tenant = claimed
			}
			if local {
				return runDataEnrollLocal(cmd, configPath, tenant, token, deviceID, csrEndpoint, environment)
			}
			return dataEnrollRemote(cmd, tenant, token, deviceID, csrEndpoint, environment)
		},
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "pki-core tenant UUID (default: the token's tenant_uuid claim)")
	cmd.Flags().StringVar(&token, "token", "", "pki-core enrollment token (required)")
	cmd.Flags().StringVar(&deviceID, "device-id", "", "device_id the token was minted with (default: the agent's certificate Common Name)")
	cmd.Flags().StringVar(&csrEndpoint, "csr-endpoint", "", "pki-core CSR frontend URL (default: derived from --environment)")
	cmd.Flags().StringVar(&environment, "environment", "", "\"dev\" or \"prod\"; selects the derived CSR frontend host")
	cmd.Flags().BoolVar(&local, "local", false, "Write the staged credential into this machine's agent config directory instead of calling the agent")
	cmd.Flags().StringVar(&configPath, "config-path", "", "agent config directory, implies --local (default: $WENDY_CONFIG_PATH or "+defaultAgentConfigPath+")")
	return cmd
}

// runDataEnrollLocal writes the credential file next to the agent on this
// machine. Nothing is contacted; the agent redeems the file on its next start.
func runDataEnrollLocal(cmd *cobra.Command, configPath, tenant, token, deviceID, csrEndpoint, environment string) error {
	if configPath == "" {
		configPath = agentConfigPath()
	}
	// Resolved before staging, not after: a frontend URL the agent will refuse
	// must not first be written to disk alongside the token.
	frontendURL, err := pkienroll.CSRFrontendURL(csrEndpoint, environment)
	if err != nil {
		return err
	}
	path, err := pkienroll.Stage(configPath, pkienroll.StagedEnrollment{
		Token:       token,
		TenantUUID:  tenant,
		DeviceID:    deviceID,
		CSREndpoint: csrEndpoint,
		Environment: environment,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(),
		"Staged pki-core enrollment token at %s\n"+
			"  tenant:   %s\n"+
			"  frontend: %s\n"+
			"The agent redeems and deletes it on its next start; restart wendy-agent to apply now.\n",
		path, tenant, frontendURL)
	return nil
}

// dataEnrollRemote is indirected so a test can assert which path the flags
// select without standing up a device.
var dataEnrollRemote = runDataEnrollRemote

// runDataEnrollRemote sends the token to the agent, which stages and redeems it
// on the device.
func runDataEnrollRemote(cmd *cobra.Command, tenant, token, deviceID, csrEndpoint, environment string) error {
	conn, err := connectToAgent(cmd.Context())
	if err != nil {
		return err
	}
	defer conn.Close()
	return stagePKIEnrollmentOverRPC(cmd, conn.ProvisioningServiceV2, tenant, token, deviceID, csrEndpoint, environment)
}

// stagePKIEnrollmentOverRPC is the call itself, split from the dial so it can
// be driven against a test server.
func stagePKIEnrollmentOverRPC(cmd *cobra.Command, client agentpbv2.WendyProvisioningServiceClient,
	tenant, token, deviceID, csrEndpoint, environment string) error {
	// Enrolment is a round trip to a certificate authority on the device's
	// side, and the agent retries a transient failure three times with five
	// seconds between attempts. The deadline has to clear that, or a slow but
	// succeeding enrolment would look like a failure here while the token was
	// spent there.
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()

	resp, err := client.StagePKIEnrollment(ctx, &agentpbv2.StagePKIEnrollmentRequest{
		TenantUuid:  tenant,
		Token:       token,
		DeviceId:    deviceID,
		CsrEndpoint: csrEndpoint,
		Environment: environment,
	})
	if err != nil {
		return err
	}
	return printDataEnrollOutcome(cmd, resp)
}

// printDataEnrollOutcome says what happened, and for anything short of success
// what to do about it. The token is single-use, so "retry" and "mint a fresh
// one" are different instructions and the difference has to be explicit.
func printDataEnrollOutcome(cmd *cobra.Command, resp *agentpbv2.StagePKIEnrollmentResponse) error {
	out := cmd.OutOrStdout()
	switch resp.GetStatus() {
	case agentpbv2.StagePKIEnrollmentResponse_STATUS_ENROLLED:
		fmt.Fprintf(out, "Enrolled pki-core device identity\n  identity: %s\n", resp.GetSpiffeUri())
		if name := resp.GetDeviceName(); name != "" {
			fmt.Fprintf(out, "  name:     %s\n", name)
		}
		if unix := resp.GetNotAfterUnix(); unix > 0 {
			fmt.Fprintf(out, "  expires:  %s\n", time.Unix(unix, 0).UTC().Format(time.RFC3339))
		}
		fmt.Fprintf(out, "  staged:   %s (redeemed and deleted)\n", resp.GetStagedPath())
		return nil
	case agentpbv2.StagePKIEnrollmentResponse_STATUS_ALREADY_ENROLLED:
		fmt.Fprintln(out, "The device already holds a pki-core identity; the token was not spent.")
		if uri := resp.GetSpiffeUri(); uri != "" {
			fmt.Fprintf(out, "  identity: %s\n", uri)
		}
		fmt.Fprintln(out, "That identity is renewed in place. Remove the stored identity if you really mean to re-enroll.")
		return nil
	case agentpbv2.StagePKIEnrollmentResponse_STATUS_DEFERRED:
		fmt.Fprintf(out, "Enrollment deferred: %s\n", resp.GetReason())
		fmt.Fprintf(out, "  staged:   %s (kept)\n", resp.GetStagedPath())
		fmt.Fprintln(out, "Enroll the device with Wendy Cloud, then restart the agent; the staged token is redeemed then.")
		return nil
	case agentpbv2.StagePKIEnrollmentResponse_STATUS_REFUSED:
		return fmt.Errorf("pki-core refused the enrollment: %s\nthe token is spent; mint a fresh one", resp.GetReason())
	default:
		return fmt.Errorf("enrollment failed: %s", resp.GetReason())
	}
}

// agentConfigPath resolves the agent's config directory the same way the agent
// does, so staging a file here and reading it there cannot disagree.
func agentConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("WENDY_CONFIG_PATH")); v != "" {
		return v
	}
	return defaultAgentConfigPath
}
