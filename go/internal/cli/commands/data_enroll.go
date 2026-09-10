package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/enrolltoken"
)

// defaultAgentConfigPath is the agent's config directory, the same default
// wendy-agent applies and overrides with WENDY_CONFIG_PATH.
const defaultAgentConfigPath = "/etc/wendy-agent"

// newDataEnrollCmd stages a pki-core enrollment token for the agent.
//
// WHY A STAGED FILE AND NOT AN RPC. The agent already takes exactly one
// one-shot provisioning input this way: agent.sh writes
// <configPath>/enrollment.json and ApplyEnrollmentFile
// (go/internal/agent/services/enrollment_file.go) redeems and deletes it at
// startup. Following that precedent keeps the credential off the control plane
// and out of any long-lived configuration file, and adds no proto surface for
// a token that is used once. This command is the operator-facing way to write
// that file, so nobody has to hand-compose JSON on a device.
//
// It runs ON the device, next to the agent whose files it writes.
func newDataEnrollCmd() *cobra.Command {
	var (
		tenant      string
		token       string
		deviceID    string
		csrEndpoint string
		environment string
		configPath  string
	)
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Stage a pki-core device enrollment token for the agent",
		Long: strings.TrimSpace(`
Stage a pki-core enrollment token so the agent enrolls a data platform device
identity on its next start.

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

This command writes a file the agent reads at startup; it does not contact
pki-core. Run it on the device.`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(token) == "" {
				return errors.New("--token is required")
			}
			if strings.TrimSpace(tenant) == "" {
				claimed, ok := enrolltoken.TenantUUIDFromToken(token)
				if !ok {
					return errors.New("--tenant is required: the token carries no tenant_uuid claim")
				}
				tenant = claimed
			}
			if configPath == "" {
				configPath = agentConfigPath()
			}
			path, err := stagePKIEnrollment(configPath, tenant, token, deviceID, csrEndpoint, environment)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Staged pki-core enrollment token at %s\n"+
					"  tenant:   %s\n"+
					"  frontend: %s\n"+
					"The agent redeems and deletes it on its next start; restart wendy-agent to apply now.\n",
				path, tenant, pkienroll.CSRFrontendURL(csrEndpoint, environment))
			return nil
		},
	}
	cmd.Flags().StringVar(&tenant, "tenant", "", "pki-core tenant UUID (default: the token's tenant_uuid claim)")
	cmd.Flags().StringVar(&token, "token", "", "pki-core enrollment token (required)")
	cmd.Flags().StringVar(&deviceID, "device-id", "", "device_id the token was minted with (default: the agent's certificate Common Name)")
	cmd.Flags().StringVar(&csrEndpoint, "csr-endpoint", "", "pki-core CSR frontend URL (default: derived from --environment)")
	cmd.Flags().StringVar(&environment, "environment", "", "\"dev\" or \"prod\"; selects the derived CSR frontend host")
	cmd.Flags().StringVar(&configPath, "config-path", "", "agent config directory (default: $WENDY_CONFIG_PATH or "+defaultAgentConfigPath+")")
	return cmd
}

// agentConfigPath resolves the agent's config directory the same way the agent
// does, so staging a file here and reading it there cannot disagree.
func agentConfigPath() string {
	if v := strings.TrimSpace(os.Getenv("WENDY_CONFIG_PATH")); v != "" {
		return v
	}
	return defaultAgentConfigPath
}

// stagePKIEnrollment writes the credential file. 0600 and atomic: it holds a
// bearer credential, and a half-written one would be redeemed and burned.
func stagePKIEnrollment(configPath, tenant, token, deviceID, csrEndpoint, environment string) (string, error) {
	if err := os.MkdirAll(configPath, 0o700); err != nil {
		return "", fmt.Errorf("creating agent config directory %s: %w", configPath, err)
	}
	payload := map[string]string{
		"token":      token,
		"tenantUUID": tenant,
	}
	for k, v := range map[string]string{
		"deviceID":    deviceID,
		"csrEndpoint": csrEndpoint,
		"environment": environment,
	} {
		if strings.TrimSpace(v) != "" {
			payload[k] = v
		}
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encoding pki enrollment file: %w", err)
	}
	path := filepath.Join(configPath, services.PKIEnrollmentFileName)
	if err := atomicfile.Write(path, append(data, '\n'), 0o600); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}
	return path, nil
}
