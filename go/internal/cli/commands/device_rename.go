package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudunenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxHostnameLen is the RFC 1035 limit for a single DNS label. Kept in sync with
// the agent-side validHostname rule in internal/agent/services/hostname.go.
const maxHostnameLen = 63

func newDeviceRenameCmd() *cobra.Command {
	var cloudGRPC string

	cmd := &cobra.Command{
		Use:   "rename [name]",
		Short: "Rename a device in Wendy Cloud and set its mDNS hostname",
		Long: "Renames a device in two places: its asset name in Wendy Cloud and its " +
			"hostname (and mDNS '.local' name) on the device itself. A single name is " +
			"applied to both. The hostname is set literally — no 'wendyos-' prefix is " +
			"added — though the interactive prompt is prepopulated with 'wendyos-' as a " +
			"starting point.\n\n" +
			"Wendy Cloud is renamed first, so a name Cloud refuses (for example, one " +
			"another device in the organization already uses) leaves the device unchanged.\n\n" +
			"The name must be a valid DNS label: it starts with a lowercase letter, " +
			"contains only lowercase letters, digits, and hyphens, does not end with a " +
			"hyphen, and is at most 63 characters.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			conn, err := connectToAgent(ctx, SuppressProvisioningHint())
			if err != nil {
				return err
			}
			defer conn.Close()

			// Provisioning state tells us which cloud asset (if any) to rename.
			provResp, err := conn.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
			if err != nil {
				return fmt.Errorf("checking provisioning status: %w", err)
			}
			prov := provResp.GetProvisioned()

			name, err := resolveRenameName(args)
			if err != nil {
				return err
			}

			// Step 1: rename the cloud asset, when the device is enrolled. Done
			// first: Cloud refuses a name another device in the organization
			// holds, and that refusal must leave the hostname unchanged too.
			var assetID any
			if prov != nil {
				if isLegacyCloudEnrollment(prov.GetOrganizationId(), prov.GetAssetId()) {
					assetID, err = prov.GetAssetId(), cloudRenameAsset(ctx, cloudGRPC, prov.GetCloudHost(), prov.GetAssetId(), name)
				} else {
					assetID, err = cloudRenameAssetV2(ctx, conn, cloudGRPC, prov.GetCloudHost(), name)
				}
				if err != nil {
					return fmt.Errorf("the Wendy Cloud rename failed, so the device's hostname was not changed: %w", err)
				}
			}

			// Step 2: set the hostname on the device (applied live and persisted
			// across reboots).
			if _, err := conn.AgentService.SetHostname(ctx, &agentpb.SetHostnameRequest{Hostname: name}); err != nil {
				if prov != nil {
					return fmt.Errorf("renamed the device to %s in Wendy Cloud, but setting its hostname failed; run 'wendy device rename %s' again to finish: %w", name, name, err)
				}
				return fmt.Errorf("setting device hostname: %w", err)
			}

			// Keep the configured default device pointing at the new hostname.
			repointDefaultDevice(name)

			if jsonOutput {
				out := map[string]any{
					"hostname":     name,
					"hostnameSet":  true,
					"cloudRenamed": prov != nil,
				}
				if prov == nil {
					out["cloudSkipped"] = "device not enrolled"
				} else {
					out["assetId"] = assetID
				}
				data, marshalErr := json.MarshalIndent(out, "", "  ")
				if marshalErr != nil {
					return marshalErr
				}
				fmt.Println(string(data))
			} else {
				fmt.Printf("Hostname set to %s (mDNS: %s.local)\n", tui.Device(name), name)
				if prov == nil {
					fmt.Println(tui.WarningMessage("Device is not enrolled in Wendy Cloud; skipped cloud rename."))
				} else {
					fmt.Printf("Renamed asset %v in Wendy Cloud.\n", assetID)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint to use (defaults to the device's enrolled cloud host)")
	return cmd
}

// resolveRenameName returns the validated new device name from the positional
// argument, or prompts for it interactively (prepopulated with "wendyos-").
func resolveRenameName(args []string) (string, error) {
	if len(args) > 0 {
		name := strings.TrimSpace(args[0])
		if err := validateHostnameArg(name); err != nil {
			return "", err
		}
		return name, nil
	}
	if !isInteractiveTerminal() {
		return "", fmt.Errorf("provide a name: 'wendy device rename <name>'")
	}
	entered, err := tui.PromptTextWithDefault(
		"New device name",
		"lowercase letters, digits, and hyphens; sets both the hostname and the cloud name",
		"wendyos-",
		validateHostnameArg,
	)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(entered), nil
}

// cloudRenameAssetV2 renames a v2 (PKI) enrollment's asset in Wendy Cloud to
// name with the operator-signed wendycloud.v2 UpdateAsset, and returns the
// asset UUID. The agent reports only the device principal; Cloud resolves it.
func cloudRenameAssetV2(ctx context.Context, conn *grpcclient.AgentConnection, cloudGRPC, deviceCloudHost, name string) (string, error) {
	state, err := agentpbv2.NewWendyProvisioningServiceClient(conn.Conn).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if err != nil && status.Code(err) != codes.Unimplemented {
		return "", fmt.Errorf("reading the device's Cloud enrollment: %w", err)
	}
	principal, err := certs.ParsePrincipal(state.GetProvisioned().GetPrincipalUri())
	if err != nil || principal.EntityType != certs.EntityAsset {
		return "", fmt.Errorf("cannot identify this device's Cloud enrollment; update the Agent with 'wendy device update' and retry")
	}
	if deviceCloudHost == "" {
		return "", fmt.Errorf("the device reports no enrolled Cloud host")
	}
	if cloudGRPC != "" && cloudGRPC != deviceCloudHost {
		return "", fmt.Errorf("--cloud-grpc %s is not the device's enrolled Cloud host %s", cloudGRPC, deviceCloudHost)
	}
	auth, err := pickAuthEntry(deviceCloudHost)
	if err != nil {
		return "", fmt.Errorf("selecting cloud auth session: %w", err)
	}
	if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != principal.TenantUUID {
		return "", fmt.Errorf("the selected Wendy Cloud session is not in the device's organization %s; select one with 'wendy auth default' or sign in with 'wendy auth login'", principal.TenantUUID)
	}
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return "", err
	}
	defer cloudConn.Close()
	rpcctx, err := cloudContext(ctx, auth)
	if err != nil {
		return "", err
	}
	tenant, device := principal.TenantUUID, principal.EntityID
	asset, err := cloudpbv2.NewAssetServiceClient(cloudConn).GetAsset(rpcctx, &cloudpbv2.GetAssetRequest{OrganizationId: &tenant, DeviceId: &device})
	if err != nil {
		return "", fmt.Errorf("looking up the device's Cloud asset: %w", err)
	}
	if asset.GetOrganizationId() != tenant || cloudunenroll.DeviceID(asset) != device {
		return "", fmt.Errorf("Wendy Cloud returned a different asset for %s", principal.Principal)
	}
	if err := cloudrequest.Invoke(rpcctx, cloudConn, auth, cloudpbv2.AssetService_UpdateAsset_FullMethodName,
		&cloudpbv2.UpdateAssetRequest{Id: asset.GetId(), Name: &name}, &cloudpbv2.Asset{}); err != nil {
		return "", fmt.Errorf("renaming asset %s: %w", asset.GetId(), err)
	}
	return asset.GetId(), nil
}

// cloudRenameAsset renames a legacy (numeric) enrollment's asset in Wendy
// Cloud to name.
func cloudRenameAsset(ctx context.Context, cloudGRPC, deviceCloudHost string, assetID int32, name string) error {
	cloudConn, tokenCtx, _, err := dialCloud(ctx, cloudGRPC, deviceCloudHost)
	if err != nil {
		return err
	}
	defer cloudConn.Close()

	assetClient := cloudpb.NewAssetServiceClient(cloudConn)
	if _, err := assetClient.UpdateAsset(tokenCtx, &cloudpb.UpdateAssetRequest{Id: assetID, Name: &name}); err != nil {
		return fmt.Errorf("renaming asset %d: %w", assetID, err)
	}
	return nil
}

// repointDefaultDevice updates the configured default device to the new
// hostname when the default was the device we just renamed. It is best-effort
// and conservative: it only fires when no explicit --device override was given
// (so the default device was the connection target) AND the default is an mDNS
// '.local' name (renaming changes the '.local' name, so the old one stops
// resolving). An IP or non-mDNS default is left untouched so we never point the
// default at a name that may not resolve on the device's network.
func repointDefaultDevice(name string) {
	if deviceFlag != "" {
		return
	}
	err := config.Update(func(cfg *config.Config) (bool, error) {
		if !strings.HasSuffix(cfg.DefaultDevice, ".local") {
			return false, nil
		}
		cfg.DefaultDevice = name + ".local"
		return true, nil
	})
	if err != nil {
		cliLogln("Warning: renamed device but could not update the default device: %v", err)
	}
}

// validateHostnameArg validates a device name as a DNS label, mirroring the
// agent-side rule in internal/agent/services/hostname.go.
func validateHostnameArg(name string) error {
	if len(name) == 0 {
		return fmt.Errorf("name must not be empty")
	}
	if len(name) > maxHostnameLen {
		return fmt.Errorf("name must be at most %d characters", maxHostnameLen)
	}
	for i, c := range name {
		switch {
		case c >= 'a' && c <= 'z':
			// always ok
		case (c >= '0' && c <= '9') || c == '-':
			if i == 0 {
				return fmt.Errorf("name must start with a lowercase letter")
			}
		default:
			return fmt.Errorf("name may only contain lowercase letters, digits, and hyphens")
		}
	}
	if name[len(name)-1] == '-' {
		return fmt.Errorf("name must not end with a hyphen")
	}
	return nil
}
