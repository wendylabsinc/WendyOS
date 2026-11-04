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
	cmd := &cobra.Command{Use: "connect", Short: "Join Wendy Net automatically and stay connected", Long: "Join Wendy Net from this computer using your selected sign-in profile. The CLI automatically finds and authenticates nearby routers over LAN and Bluetooth, can keep multiple connections, and stays connected between commands. Use wendy discover to choose a destination device. No router address or Bluetooth identifier is needed. Run wendy device wendy-net disconnect to leave the mesh.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		auth, err := config.ResolveAuth(cfg, "", nil)
		if err != nil {
			return err
		}
		if len(auth.Certificates) == 0 {
			return fmt.Errorf("the selected sign-in profile has no certificate for joining Wendy Net; run 'wendy auth login' for your organization")
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
	return &cobra.Command{Use: "disconnect", Short: "Disconnect this computer from Wendy Net", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return sessionbroker.DisconnectOperator(cmd.Context()) }}
}
