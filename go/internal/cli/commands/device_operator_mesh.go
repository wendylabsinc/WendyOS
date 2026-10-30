package commands

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// This explicit preparation is separate from device selection: the first hop
// is configuration for the retained operator context, not a remote identity.
func newDeviceOperatorMeshConnectCmd() *cobra.Command {
	var address, router, transport string
	var psm uint16
	cmd := &cobra.Command{Use: "connect", Short: "Keep an operator mesh attachment ready for ordinary commands", Long: "Prepare the current profile's background operator context through a discovered WendyOS or PSRAM Lite TLS router, or select one with --address and --router. Subsequent commands may select reachable WendyOS or Lite devices with mesh://TENANT/DEVICE. One background router owns Babel, authenticated carriers and the signed directory across commands. Automatic discovery uses LAN TLS and BLE; Nearby shows signed endpoints with usable routes. An explicit pinned connection remains enabled until disconnect.", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
		if transport != "tcp" && transport != "ble" {
			return fmt.Errorf("--transport must be tcp or ble")
		}
		if address == "" && (transport == "ble" || psm != 0) {
			return fmt.Errorf("BLE first-hop selection requires --address and --router")
		}
		if (address == "") != (router == "") {
			return fmt.Errorf("--address and --router must be supplied together")
		}
		prepare := sessionbroker.EnableOperatorAutomatic
		if address != "" {
			prepare = func(ctx context.Context, certificate config.CertificateInfo) (*grpcclient.AgentConnection, error) {
				return sessionbroker.PrepareOperatorTransport(ctx, certificate, address, router, transport, psm)
			}
		}
		client, err := prepare(cmd.Context(), auth.Certificates[0])
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
	cmd.Flags().StringVar(&address, "address", "", "Adjacent router TLS host:port or BLE peripheral UUID/LE address")
	cmd.Flags().StringVar(&transport, "transport", "tcp", "Explicit first-hop carrier: tcp or ble; discovery uses both when no address is supplied")
	cmd.Flags().Uint16Var(&psm, "psm", 0, "Explicit BLE dynamic PSM (default 129)")
	cmd.Flags().StringVar(&router, "router", "", "Exact enrolled device UUID of the adjacent router")
	return cmd
}

func newDeviceOperatorMeshDisconnectCmd() *cobra.Command {
	return &cobra.Command{Use: "disconnect", Short: "Close this operator's retained mesh context", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return sessionbroker.DisconnectOperator(cmd.Context()) }}
}
