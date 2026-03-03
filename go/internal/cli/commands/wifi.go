package commands

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/ble"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newWifiCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wifi",
		Short: "Manage WiFi on the target device",
	}

	cmd.AddCommand(
		newWifiListCmd(),
		newWifiConnectCmd(),
		newWifiStatusCmd(),
		newWifiDisconnectCmd(),
	)

	return cmd
}

func newWifiListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available WiFi networks",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			// BLE WendyOS agent path
			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				return wifiListViaBLEAgent(target.Bluetooth)
			}

			// BLE Wendy Lite — no network scan support
			if target.Bluetooth != nil {
				return fmt.Errorf("Wendy Lite devices do not support listing WiFi networks; use 'wendy wifi connect' with --ssid instead")
			}

			// gRPC LAN path
			if target.Agent == nil {
				return fmt.Errorf("selected device does not support WiFi network listing")
			}

			resp, err := target.Agent.AgentService.ListWiFiNetworks(ctx, &agentpb.ListWiFiNetworksRequest{})
			if err != nil {
				return fmt.Errorf("listing WiFi networks: %w", err)
			}

			networks := resp.GetNetworks()
			if jsonOutput {
				data, err := json.MarshalIndent(networks, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			if len(networks) == 0 {
				fmt.Println("No WiFi networks found.")
				return nil
			}

			headers := []string{"SSID", "Signal"}
			var rows [][]string
			for _, n := range networks {
				rows = append(rows, []string{
					n.GetSsid(),
					fmt.Sprintf("%d%%", n.GetSignalStrength()),
				})
			}
			fmt.Print(tui.RenderTable(headers, rows))
			return nil
		},
	}
}

func newWifiConnectCmd() *cobra.Command {
	var ssid string
	var password string

	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Connect to a WiFi network",
		RunE: func(cmd *cobra.Command, args []string) error {
			if ssid == "" {
				return fmt.Errorf("--ssid is required")
			}

			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			// BLE WendyOS agent path (protobuf over L2CAP)
			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				return wifiConnectViaBLEAgent(target.Bluetooth, ssid, password)
			}

			// BLE Wendy Lite path (GATT provisioning)
			if target.Bluetooth != nil {
				return wifiConnectViaBLELite(target.Bluetooth, ssid, password)
			}

			// gRPC LAN path
			if target.Agent == nil {
				return fmt.Errorf("selected device does not support WiFi connect")
			}

			resp, err := target.Agent.AgentService.ConnectToWiFi(ctx, &agentpb.ConnectToWiFiRequest{
				Ssid:     ssid,
				Password: password,
			})
			if err != nil {
				return fmt.Errorf("connecting to WiFi: %w", err)
			}

			if !resp.GetSuccess() {
				return fmt.Errorf("failed to connect: %s", resp.GetErrorMessage())
			}

			fmt.Printf("Connected to %s\n", ssid)
			return nil
		},
	}

	cmd.Flags().StringVar(&ssid, "ssid", "", "WiFi network SSID")
	cmd.Flags().StringVar(&password, "password", "", "WiFi network password")

	return cmd
}

func newWifiStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Get current WiFi connection status",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			// BLE WendyOS agent path
			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				return wifiStatusViaBLEAgent(target.Bluetooth)
			}

			// BLE Wendy Lite path
			if target.Bluetooth != nil {
				return wifiStatusViaBLELite(target.Bluetooth)
			}

			// gRPC LAN path
			if target.Agent == nil {
				return fmt.Errorf("selected device does not support WiFi status")
			}

			resp, err := target.Agent.AgentService.GetWiFiStatus(ctx, &agentpb.GetWiFiStatusRequest{})
			if err != nil {
				return fmt.Errorf("getting WiFi status: %w", err)
			}

			if jsonOutput {
				data, err := json.MarshalIndent(map[string]interface{}{
					"connected": resp.GetConnected(),
					"ssid":      resp.GetSsid(),
				}, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(data))
				return nil
			}

			if resp.GetConnected() {
				fmt.Printf("Connected to: %s\n", resp.GetSsid())
			} else {
				fmt.Println("Not connected to any WiFi network.")
			}
			return nil
		},
	}
}

func newWifiDisconnectCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "disconnect",
		Short: "Disconnect from the current WiFi network",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			// BLE WendyOS agent path
			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				return wifiDisconnectViaBLEAgent(target.Bluetooth)
			}

			// BLE Wendy Lite — disconnect not supported, but clear credentials is
			if target.Bluetooth != nil {
				return wifiDisconnectViaBLELite(target.Bluetooth)
			}

			// gRPC LAN path
			if target.Agent == nil {
				return fmt.Errorf("selected device does not support WiFi disconnect")
			}

			resp, err := target.Agent.AgentService.DisconnectWiFi(ctx, &agentpb.DisconnectWiFiRequest{})
			if err != nil {
				return fmt.Errorf("disconnecting WiFi: %w", err)
			}

			if !resp.GetSuccess() {
				return fmt.Errorf("failed to disconnect: %s", resp.GetErrorMessage())
			}

			fmt.Println("Disconnected from WiFi.")
			return nil
		},
	}
}

// ── BLE WendyOS Agent helpers (protobuf over L2CAP) ────────────────

func wifiConnectViaBLEAgent(device *models.BluetoothDevice, ssid, password string) error {
	fmt.Printf("Connecting to %s via Bluetooth...\n", device.DisplayName)

	client, err := ble.ConnectAgent(device)
	if err != nil {
		return err
	}
	defer client.Close()

	fmt.Printf("Sending WiFi credentials for '%s'...\n", ssid)
	if err := client.WifiConnect(ssid, password); err != nil {
		return err
	}

	fmt.Printf("Connected to %s\n", ssid)
	return nil
}

func wifiListViaBLEAgent(device *models.BluetoothDevice) error {
	fmt.Printf("Connecting to %s via Bluetooth...\n", device.DisplayName)

	client, err := ble.ConnectAgent(device)
	if err != nil {
		return err
	}
	defer client.Close()

	networks, err := client.WifiList()
	if err != nil {
		return fmt.Errorf("listing WiFi networks: %w", err)
	}

	if jsonOutput {
		data, err := json.MarshalIndent(networks, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if len(networks) == 0 {
		fmt.Println("No WiFi networks found.")
		return nil
	}

	headers := []string{"SSID", "Signal"}
	var rows [][]string
	for _, n := range networks {
		rows = append(rows, []string{
			n.GetSsid(),
			fmt.Sprintf("%d%%", n.GetSignalStrength()),
		})
	}
	fmt.Print(tui.RenderTable(headers, rows))
	return nil
}

func wifiStatusViaBLEAgent(device *models.BluetoothDevice) error {
	fmt.Printf("Connecting to %s via Bluetooth...\n", device.DisplayName)

	client, err := ble.ConnectAgent(device)
	if err != nil {
		return err
	}
	defer client.Close()

	resp, err := client.WifiStatus()
	if err != nil {
		return fmt.Errorf("getting WiFi status: %w", err)
	}

	if jsonOutput {
		data, err := json.MarshalIndent(map[string]interface{}{
			"connected": resp.GetConnected(),
			"ssid":      resp.GetSsid(),
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if resp.GetConnected() {
		fmt.Printf("Connected to: %s\n", resp.GetSsid())
	} else {
		fmt.Println("Not connected to any WiFi network.")
	}
	return nil
}

func wifiDisconnectViaBLEAgent(device *models.BluetoothDevice) error {
	fmt.Printf("Connecting to %s via Bluetooth...\n", device.DisplayName)

	client, err := ble.ConnectAgent(device)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.WifiDisconnect(); err != nil {
		return err
	}

	fmt.Println("Disconnected from WiFi.")
	return nil
}

// ── BLE Wendy Lite helpers (GATT provisioning) ─────────────────────

func wifiConnectViaBLELite(device *models.BluetoothDevice, ssid, password string) error {
	fmt.Printf("Connecting to %s via Bluetooth...\n", device.DisplayName)

	client, err := ble.ConnectLite(device)
	if err != nil {
		return err
	}
	defer client.Close()

	fmt.Printf("Provisioning WiFi '%s' on %s...\n", ssid, device.DisplayName)
	result, err := client.WifiConnect(ssid, password)
	if err != nil {
		return err
	}

	if result.Connected {
		if result.IPAddress != "" {
			fmt.Printf("Connected to %s (IP: %s)\n", ssid, result.IPAddress)
		} else {
			fmt.Printf("Connected to %s\n", ssid)
		}
	} else {
		return fmt.Errorf("failed to connect to %s", ssid)
	}

	return nil
}

func wifiStatusViaBLELite(device *models.BluetoothDevice) error {
	fmt.Printf("Connecting to %s via Bluetooth...\n", device.DisplayName)

	client, err := ble.ConnectLite(device)
	if err != nil {
		return err
	}
	defer client.Close()

	result, err := client.WifiStatus()
	if err != nil {
		return err
	}

	if jsonOutput {
		data, err := json.MarshalIndent(map[string]interface{}{
			"connected": result.Connected,
			"ipAddress": result.IPAddress,
		}, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}

	if result.Connected {
		if result.IPAddress != "" {
			fmt.Printf("Connected (IP: %s)\n", result.IPAddress)
		} else {
			fmt.Println("Connected to WiFi.")
		}
	} else {
		fmt.Println("Not connected to any WiFi network.")
	}
	return nil
}

func wifiDisconnectViaBLELite(device *models.BluetoothDevice) error {
	fmt.Printf("Connecting to %s via Bluetooth...\n", device.DisplayName)

	client, err := ble.ConnectLite(device)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.WifiClearCredentials(); err != nil {
		return fmt.Errorf("clearing WiFi credentials: %w", err)
	}

	fmt.Println("WiFi credentials cleared. Device will disconnect from WiFi.")
	return nil
}
