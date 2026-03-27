package commands

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/ble"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"golang.org/x/term"
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
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"))
			if err != nil {
				return err
			}
			defer target.Close()

			// BLE WendyOS agent path
			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				return wifiListViaBLEAgent(target.Bluetooth)
			}

			// BLE Wendy Lite — scan from the host machine
			if target.Bluetooth != nil {
				return wifiListFromHost()
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
			ctx := cmd.Context()
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"))
			if err != nil {
				return err
			}
			defer target.Close()

			// If no SSID provided, scan for networks and let the user pick.
			if ssid == "" {
				picked, pickErr := pickWifiNetwork(ctx, target)
				if pickErr != nil {
					return pickErr
				}
				ssid = picked
			}

			// If no password provided and terminal is interactive, offer keychain
			// lookup (macOS only) or fall back to manual entry.
			if !cmd.Flags().Changed("password") && term.IsTerminal(int(os.Stdin.Fd())) {
				if supportsKeychainLookup {
					fmt.Printf("Look up password for '%s' from keychain? (macOS will ask for permission) [Y/n] ", ssid)
					reader := bufio.NewReader(os.Stdin)
					line, _ := reader.ReadString('\n')
					answer := strings.TrimSpace(strings.ToLower(line))

					if answer == "" || answer == "y" || answer == "yes" {
						if kp, err := lookupKeychainPassword(ssid); err == nil && kp != "" {
							fmt.Println("Using saved password from keychain.")
							password = kp
						} else {
							fmt.Println("Password not available from keychain.")
						}
					}
				}

				// If still no password, prompt for manual entry.
				if password == "" {
					fmt.Print("Password (leave empty for open networks): ")
					passwordBytes, readErr := term.ReadPassword(int(os.Stdin.Fd()))
					fmt.Println()
					if readErr != nil {
						return fmt.Errorf("reading password: %w", readErr)
					}
					password = strings.TrimSpace(string(passwordBytes))
				}
			}

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
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"))
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
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"))
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

// ── WiFi network picker ─────────────────────────────────────────────

// pickWifiNetwork scans for WiFi networks on the target device and presents
// an interactive picker. Returns the selected SSID.
func pickWifiNetwork(ctx context.Context, target *SelectedDevice) (string, error) {
	type wifiEntry struct {
		ssid           string
		signalStrength int32
	}

	var networks []wifiEntry

	switch {
	case target.Bluetooth != nil && target.Bluetooth.IsWendyAgent():
		fmt.Printf("Scanning for WiFi networks on %s...\n", target.Bluetooth.DisplayName)
		client, err := ble.ConnectAgent(target.Bluetooth)
		if err != nil {
			return "", fmt.Errorf("connecting to device: %w", err)
		}
		defer client.Close()

		nets, err := client.WifiList()
		if err != nil {
			return "", fmt.Errorf("listing WiFi networks: %w", err)
		}
		for _, n := range nets {
			networks = append(networks, wifiEntry{ssid: n.GetSsid(), signalStrength: n.GetSignalStrength()})
		}

	case target.Bluetooth != nil:
		fmt.Println("Scanning for WiFi networks on this computer...")
		nets, err := scanLocalWifiNetworks()
		if err != nil {
			return "", fmt.Errorf("scanning local WiFi networks: %w", err)
		}
		for _, n := range nets {
			networks = append(networks, wifiEntry{ssid: n.SSID, signalStrength: n.SignalStrength})
		}

	case target.Agent != nil:
		fmt.Println("Scanning for WiFi networks...")
		resp, err := target.Agent.AgentService.ListWiFiNetworks(ctx, &agentpb.ListWiFiNetworksRequest{})
		if err != nil {
			return "", fmt.Errorf("listing WiFi networks: %w", err)
		}
		for _, n := range resp.GetNetworks() {
			networks = append(networks, wifiEntry{ssid: n.GetSsid(), signalStrength: n.GetSignalStrength()})
		}

	default:
		return "", fmt.Errorf("selected device does not support WiFi network scanning")
	}

	if len(networks) == 0 {
		return "", fmt.Errorf("no WiFi networks found")
	}

	// Build picker items from the scan results.
	var items []tui.PickerItem
	for _, n := range networks {
		signal := ""
		if n.signalStrength > 0 {
			signal = fmt.Sprintf("%d%%", n.signalStrength)
		}
		items = append(items, tui.PickerItem{
			Name:  n.ssid,
			Type:  signal,
			Value: n.ssid,
		})
	}

	picker := tui.NewPickerWithTitle("Select a WiFi network")
	p := tea.NewProgram(picker)

	go func() {
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("network picker: %w", err)
	}

	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return "", ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return "", fmt.Errorf("no network selected")
	}

	ssid, ok := sel.Value.(string)
	if !ok {
		return "", fmt.Errorf("invalid picker selection")
	}
	return ssid, nil
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

// ── Local host WiFi scan (for Wendy Lite) ──────────────────────────

func wifiListFromHost() error {
	fmt.Println("Scanning for WiFi networks on this computer...")
	networks, err := scanLocalWifiNetworks()
	if err != nil {
		return err
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
		signal := ""
		if n.SignalStrength > 0 {
			signal = fmt.Sprintf("%d%%", n.SignalStrength)
		}
		rows = append(rows, []string{n.SSID, signal})
	}
	fmt.Print(tui.RenderTable(headers, rows))
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
