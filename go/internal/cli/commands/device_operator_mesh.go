package commands

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Joining the local mesh is independent of selecting a destination device.
func newDeviceOperatorMeshConnectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "connect", Short: "Keep an operator mesh attachment ready for ordinary commands", Long: "Enable the current profile's background operator router. It automatically discovers and authenticates mesh neighbours over LAN and Bluetooth and maintains connections across commands. Use wendy discover to find reachable devices, then select a destination with mesh://TENANT/DEVICE. The router remains enabled until disconnect.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		auth, err := config.ResolveAuth(cfg, "", nil)
		if err != nil {
			return err
		}
		if len(auth.Certificates) == 0 {
			return fmt.Errorf("current context has no primary operator certificate")
		}
		client, err := sessionbroker.EnableOperatorAutomatic(cmd.Context(), auth.Certificates[0])
		if err != nil {
			return err
		}
		defer client.Close()
		if jsonOutput {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Selector  string `json:"selector"`
				Transport string `json:"transport"`
			}{client.Host, "operator-attachment"})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Wendy Net is connected. Use wendy discover to select a reachable device.\n")
		return err
	}}
	return cmd
}

func newDeviceOperatorMeshDisconnectCmd() *cobra.Command {
	return &cobra.Command{Use: "disconnect", Short: "Close this operator's retained mesh context", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return sessionbroker.DisconnectOperator(cmd.Context()) }}
}
