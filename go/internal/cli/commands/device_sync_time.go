package commands

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	clitimesync "github.com/wendylabsinc/wendy/go/internal/cli/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func newDeviceSyncTimeCmd() *cobra.Command {
	return &cobra.Command{
		Hidden: true,
		Use:    "sync-time",
		Short:  "Sync WendyOS clocks or a USB-connected Wendy Lite clock using Roughtime",
		Long: `Queries a Roughtime server for a cryptographically signed timestamp,
then multicasts the signed proof to all WendyOS devices on the local network.
Devices verify the Roughtime signature themselves. For Wendy Lite, select its USB
connection with --device wendy-lite:/dev/cu.usbmodemXXXX. The CLI relays fresh,
device-nonce-bound replies; the firmware requires two agreeing pinned servers.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			selector := mcpStartupDevice(deviceFlag, cfg)
			if strings.HasPrefix(selector, "wendy-lite:") {
				target, err := resolveTarget(cmd.Context(), SelectDevice(selector), SuppressProvisioningHint(), SuppressPickerEnroll())
				if err != nil {
					return err
				}
				if target.External == nil || target.External.ProviderKey != "wendy-lite" || target.External.ConnectionType() != "USB" {
					return fmt.Errorf("Wendy Lite time sync requires its physical USB connection")
				}
				client := liteclient.NewWendyLiteClient()
				if err := client.ConnectToSerial(target.External.ConnectionInfo["serialPort"]); err != nil {
					return err
				}
				defer client.Close()
				when, err := client.SyncTime(cmd.Context())
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Wendy Lite clock synchronized to %s using verified Roughtime consensus.\n", when.UTC().Format(time.RFC3339))
				return nil
			}
			result, err := clitimesync.BroadcastTime(cmd.Context())
			if err != nil {
				return err
			}
			source := "live query"
			if cached, age := clitimesync.ProofFromCache(); cached {
				source = fmt.Sprintf("proof cached %s ago — no route to a Roughtime server",
					age.Round(time.Minute))
			}
			fmt.Printf("Broadcast: %s ± %s  (via %s, %s)\n",
				result.Midpoint.UTC().Format("2006-01-02T15:04:05Z"),
				result.Radius.Round(time.Millisecond),
				result.Server, source)
			return nil
		},
	}
}
