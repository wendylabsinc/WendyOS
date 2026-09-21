package commands

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
	clitimesync "github.com/wendylabsinc/wendy/go/internal/cli/timesync"
)

func newDeviceSyncTimeCmd() *cobra.Command {
	return &cobra.Command{
		Hidden: true,
		Use:    "sync-time",
		Short:  "Sync the clock on nearby WendyOS devices via Roughtime multicast",
		Long: `Queries a Roughtime server for a cryptographically signed timestamp,
then multicasts the signed proof to all WendyOS devices on the local network.
Devices verify the Roughtime signature themselves — the Mac is an untrusted relay.`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
