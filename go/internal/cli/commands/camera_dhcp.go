package commands

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newCameraDHCPCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "dhcp", Short: "Manage address assignment for directly connected cameras",
		Long: `Camera DHCP lets a WendyOS device assign addresses to cameras on a dedicated
Ethernet port. It is disabled by default. Enable it only on ports connected
directly to cameras, not on a shared LAN or an uplink.

Use status to find ports and see whether each is disabled, waiting, serving,
or blocked. Settings persist across agent restarts and OS updates.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runCameraDHCP(cmd, "status", "") },
	}
	cmd.AddCommand(&cobra.Command{
		Use: "status", Short: "Show camera DHCP configuration and live port status", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runCameraDHCP(cmd, "status", "") },
	})
	for _, action := range []string{"enable", "disable"} {
		short := "Enable address assignment on a dedicated camera port"
		if action == "disable" {
			short = "Disable address assignment and stop camera DHCP on a port"
		}
		cmd.AddCommand(&cobra.Command{
			Use: action + " <interface>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error { return runCameraDHCP(cmd, action, args[0]) },
		})
	}
	return cmd
}

func runCameraDHCP(cmd *cobra.Command, action, iface string) error {
	ctx := cmd.Context()
	target, err := resolveCameraTargetFn(ctx)
	if err != nil {
		return err
	}
	defer target.Close()
	if isLiteCameraTarget(target) {
		return fmt.Errorf("camera DHCP is managed by a WendyOS device, not a Wendy Lite camera")
	}
	conn, err := connectFromSelectedDevice(target, resolveConfig{})
	if err != nil {
		return err
	}
	if err := cameraServicePreflight(ctx, conn, cameraNetworkManagement); err != nil {
		return err
	}
	var result *agentpb.CameraDHCPStatus
	if action == "status" {
		result, err = conn.VideoService.GetCameraDHCPStatus(ctx, &agentpb.GetCameraDHCPStatusRequest{})
	} else {
		result, err = conn.VideoService.SetCameraDHCP(ctx, &agentpb.SetCameraDHCPRequest{
			InterfaceName: iface, Enabled: action == "enable",
		})
	}
	if status.Code(err) == codes.Unimplemented {
		return fmt.Errorf("this agent does not support camera DHCP controls; update the agent on the target device")
	}
	if err != nil {
		return fmt.Errorf("camera DHCP %s: %w", action, err)
	}
	if !jsonOutput && action != "status" {
		if action == "enable" {
			fmt.Fprintf(cmd.OutOrStdout(), "Camera DHCP enabled on %s. Serving starts when the link passes its safety checks (checked every 15 seconds).\n\n", iface)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Camera DHCP disabled on %s.\n\n", iface)
		}
	}
	return renderCameraDHCPStatus(cmd, result)
}

func renderCameraDHCPStatus(cmd *cobra.Command, result *agentpb.CameraDHCPStatus) error {
	out := cmd.OutOrStdout()
	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	if result.GetConfigurationError() != "" {
		fmt.Fprintf(out, "Camera DHCP configuration error: %s\n", result.GetConfigurationError())
	}
	if result.GetInterfaceError() != "" {
		fmt.Fprintf(out, "Interface status unavailable: %s\n", result.GetInterfaceError())
	}
	var rows [][]string
	for _, iface := range result.GetInterfaces() {
		enabled, link := "no", "disconnected"
		if iface.GetEnabled() {
			enabled = "yes"
		}
		if iface.GetUp() && iface.GetCarrier() {
			link = "connected"
		}
		if !iface.GetPresent() {
			link = "missing"
		}
		if result.GetInterfaceError() != "" {
			link = "unknown"
		}
		addresses := strings.Join(iface.GetIpv4Addresses(), ", ")
		if addresses == "" {
			addresses = "-"
		}
		rows = append(rows, []string{iface.GetInterfaceName(), enabled, link, addresses, iface.GetState(), iface.GetDetail()})
	}
	if len(rows) != 0 {
		fmt.Fprint(out, tui.RenderTable([]string{"Interface", "Enabled", "Link", "IPv4", "State", "Detail"}, rows))
	} else if result.GetInterfaceError() == "" {
		fmt.Fprintln(out, "No wired camera ports found.")
	}
	fmt.Fprintln(out, "\nFor a dedicated camera port: wendy device camera dhcp enable <interface>")
	fmt.Fprintln(out, "To stop serving addresses:   wendy device camera dhcp disable <interface>")
	return nil
}
