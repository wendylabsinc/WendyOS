package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
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

			// Step 1: set the hostname on the device (applied live and persisted
			// across reboots). Done first: if this fails we leave the cloud untouched.
			if _, err := conn.AgentService.SetHostname(ctx, &agentpb.SetHostnameRequest{Hostname: name}); err != nil {
				return fmt.Errorf("setting device hostname: %w", err)
			}

			// Step 2: rename the cloud asset, when the device is enrolled.
			var cloudErr error
			cloudRenamed := false
			if prov != nil {
				cloudErr = cloudRenameAsset(ctx, cloudGRPC, prov.GetCloudHost(), prov.GetAssetId(), name)
				cloudRenamed = cloudErr == nil
			}

			// Keep the configured default device pointing at the new hostname.
			repointDefaultDevice(name)

			if jsonOutput {
				out := map[string]any{
					"hostname":     name,
					"hostnameSet":  true,
					"cloudRenamed": cloudRenamed,
				}
				if prov == nil {
					out["cloudSkipped"] = "device not enrolled"
				} else {
					out["assetId"] = prov.GetAssetId()
				}
				if cloudErr != nil {
					out["cloudError"] = cloudErr.Error()
				}
				data, marshalErr := json.MarshalIndent(out, "", "  ")
				if marshalErr != nil {
					return marshalErr
				}
				fmt.Println(string(data))
			} else {
				fmt.Printf("Hostname set to %s (mDNS: %s.local)\n", tui.Device(name), name)
				switch {
				case prov == nil:
					fmt.Println(tui.WarningMessage("Device is not enrolled in Wendy Cloud; skipped cloud rename."))
				case cloudErr != nil:
					fmt.Println(tui.WarningMessage(fmt.Sprintf("Hostname updated, but the Wendy Cloud rename failed: %v", cloudErr)))
				default:
					fmt.Printf("Renamed asset %d in Wendy Cloud.\n", prov.GetAssetId())
				}
			}

			if cloudErr != nil {
				return cloudErr
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

// cloudRenameAsset renames the device's asset in Wendy Cloud to name.
func cloudRenameAsset(ctx context.Context, cloudGRPC, deviceCloudHost string, assetID int32, name string) error {
	cloudConn, tokenCtx, err := dialCloud(ctx, cloudGRPC, deviceCloudHost)
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
