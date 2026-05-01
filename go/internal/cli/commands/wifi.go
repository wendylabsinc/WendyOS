package commands

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/ble"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/cli/tui/wifitable"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"golang.org/x/term"
)

func newWifiCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "wifi",
		Short: "Manage WiFi on the target device",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWifiInteractive(cmd)
		},
	}

	cmd.AddCommand(
		newWifiListCmd(),
		newWifiConnectCmd(),
		newWifiStatusCmd(),
		newWifiDisconnectCmd(),
		newWifiRankCmd(),
		newWifiForgetCmd(),
	)

	return cmd
}

// ── Interactive TUI entry point ─────────────────────────────────────

func runWifiInteractive(cmd *cobra.Command) error {
	ctx := cmd.Context()
	target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"))
	if err != nil {
		return err
	}
	defer target.Close()

	client, err := newWifiClient(target)
	if err != nil {
		return err
	}
	defer client.Close()

	networks, err := client.List(ctx)
	if err != nil {
		return err
	}

	model := wifitable.NewModel(networksToView(networks)).WithHandler(&wifiTUIHandler{ctx: ctx, client: client})
	p := tea.NewProgram(model)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("wifi TUI: %w", err)
	}
	return nil
}

func networksToView(networks []*agentpb.ListWiFiNetworksResponse_WiFiNetwork) []wifitable.Network {
	view := make([]wifitable.Network, 0, len(networks))
	for _, n := range networks {
		view = append(view, wifitable.FromProto(n))
	}
	return view
}

// wifiTUIHandler adapts *wifiClient to the wifitable.Handler interface so the
// TUI can execute operations inline and stay open between edits.
type wifiTUIHandler struct {
	ctx    context.Context
	client *wifiClient
}

func (h *wifiTUIHandler) Connect(ssid, password string, sec agentpb.WiFiSecurityType, hidden bool) tea.Cmd {
	return func() tea.Msg {
		req := &agentpb.ConnectToWiFiRequest{Ssid: ssid, Password: password}
		if sec != agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED {
			s := sec
			req.Security = &s
		}
		if hidden {
			hid := true
			req.Hidden = &hid
		}
		err := h.client.Connect(h.ctx, req)
		action := wifitable.ActionConnect
		if hidden {
			action = wifitable.ActionConnectUnlisted
		}
		return wifitable.OpResultMsg{Action: action, SSID: ssid, Err: err}
	}
}

func (h *wifiTUIHandler) Forget(ssid string) tea.Cmd {
	return func() tea.Msg {
		err := h.client.Forget(h.ctx, ssid)
		return wifitable.OpResultMsg{Action: wifitable.ActionForget, SSID: ssid, Err: err}
	}
}

func (h *wifiTUIHandler) Reorder(order []string) tea.Cmd {
	return func() tea.Msg {
		err := h.client.Reorder(h.ctx, order)
		return wifitable.OpResultMsg{Action: wifitable.ActionReorder, Count: len(order), Err: err}
	}
}

func (h *wifiTUIHandler) Refresh() tea.Cmd {
	return func() tea.Msg {
		nets, err := h.client.List(h.ctx)
		if err != nil {
			// Surface the error without clobbering the list.
			return wifitable.OpResultMsg{Action: wifitable.ActionNone, Err: err}
		}
		return wifitable.RefreshMsg{Networks: networksToView(nets)}
	}
}

// ── wifiClient: small abstraction over the three transports ────────

type wifiClient struct {
	ctx context.Context

	// Exactly one of these is set per instance.
	agent agentpb.WendyAgentServiceClient
	ble   *ble.AgentClient

	// shared
	target *SelectedDevice
}

func newWifiClient(target *SelectedDevice) (*wifiClient, error) {
	switch {
	case target.Bluetooth != nil && target.Bluetooth.IsWendyAgent():
		tlsCfg, err := bleTLSConfig()
		if err != nil {
			return nil, err
		}
		client, err := ble.ConnectAgent(target.Bluetooth, tlsCfg)
		if err != nil {
			return nil, fmt.Errorf("connecting to %s: %w", target.Bluetooth.DisplayName, err)
		}
		return &wifiClient{target: target, ble: client}, nil
	case target.Bluetooth != nil:
		return nil, fmt.Errorf("the interactive WiFi table requires a WendyOS agent; Wendy Lite is not supported here")
	case target.Agent != nil:
		return &wifiClient{target: target, agent: target.Agent.AgentService}, nil
	}
	return nil, fmt.Errorf("selected device does not support WiFi management")
}

func (c *wifiClient) Close() {
	if c.ble != nil {
		c.ble.Close()
	}
}

func (c *wifiClient) List(ctx context.Context) ([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, error) {
	if c.agent != nil {
		resp, err := c.agent.ListWiFiNetworks(ctx, &agentpb.ListWiFiNetworksRequest{})
		if err != nil {
			return nil, fmt.Errorf("listing WiFi networks: %w", err)
		}
		return resp.GetNetworks(), nil
	}
	nets, err := c.ble.WifiList()
	if err != nil {
		return nil, fmt.Errorf("listing WiFi networks: %w", err)
	}
	out := make([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork, 0, len(nets))
	for _, n := range nets {
		out = append(out, &agentpb.ListWiFiNetworksResponse_WiFiNetwork{
			Ssid:           n.GetSsid(),
			SignalStrength: n.SignalStrength,
			Security:       n.GetSecurity(),
			IsKnown:        n.GetIsKnown(),
			IsConnected:    n.GetIsConnected(),
			Priority:       n.Priority,
			RssiDbm:        n.RssiDbm,
		})
	}
	return out, nil
}

func (c *wifiClient) Connect(ctx context.Context, req *agentpb.ConnectToWiFiRequest) error {
	if c.agent != nil {
		resp, err := c.agent.ConnectToWiFi(ctx, req)
		if err != nil {
			return fmt.Errorf("connecting to WiFi: %w", err)
		}
		if !resp.GetSuccess() {
			return fmt.Errorf("failed to connect: %s", resp.GetErrorMessage())
		}
		return nil
	}
	sec := agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED
	if req.Security != nil {
		sec = *req.Security
	}
	hidden := false
	if req.Hidden != nil {
		hidden = *req.Hidden
	}
	return c.ble.WifiConnectWith(req.GetSsid(), req.GetPassword(), sec, hidden)
}

func (c *wifiClient) Reorder(ctx context.Context, order []string) error {
	if c.agent != nil {
		resp, err := c.agent.ReorderKnownWiFiNetworks(ctx, &agentpb.ReorderKnownWiFiNetworksRequest{OrderSsids: order})
		if err != nil {
			return fmt.Errorf("reordering WiFi networks: %w", err)
		}
		if !resp.GetSuccess() {
			return fmt.Errorf("reorder failed: %s", resp.GetErrorMessage())
		}
		return nil
	}
	return c.ble.WifiReorder(order)
}

func (c *wifiClient) SetPriority(ctx context.Context, ssid string, priority int32) error {
	if c.agent != nil {
		resp, err := c.agent.SetWiFiNetworkPriority(ctx, &agentpb.SetWiFiNetworkPriorityRequest{Ssid: ssid, Priority: priority})
		if err != nil {
			return fmt.Errorf("setting priority: %w", err)
		}
		if !resp.GetSuccess() {
			return fmt.Errorf("set priority failed: %s", resp.GetErrorMessage())
		}
		return nil
	}
	return c.ble.WifiSetPriority(ssid, priority)
}

func (c *wifiClient) Forget(ctx context.Context, ssid string) error {
	if c.agent != nil {
		resp, err := c.agent.ForgetWiFiNetwork(ctx, &agentpb.ForgetWiFiNetworkRequest{Ssid: ssid})
		if err != nil {
			return fmt.Errorf("forgetting network: %w", err)
		}
		if !resp.GetSuccess() {
			return fmt.Errorf("forget failed: %s", resp.GetErrorMessage())
		}
		return nil
	}
	return c.ble.WifiForget(ssid)
}

// ── Subcommands ────────────────────────────────────────────────────

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

			// Wendy Lite — scan from the host machine (no known/priority info).
			if target.Bluetooth != nil && !target.Bluetooth.IsWendyAgent() {
				return wifiListFromHost()
			}

			client, err := newWifiClient(target)
			if err != nil {
				return err
			}
			defer client.Close()

			networks, err := client.List(ctx)
			if err != nil {
				return err
			}

			if jsonOutput {
				return printNetworksJSON(networks)
			}

			if len(networks) == 0 {
				cliNotice("No WiFi networks found.")
				return nil
			}

			headers := []string{"SSID", "Known", "Status", "Security", "Signal"}
			var rows [][]string
			view := make([]wifitable.Network, 0, len(networks))
			for _, n := range networks {
				view = append(view, wifitable.FromProto(n))
			}
			wifitable.Sort(view)
			for _, n := range view {
				known := ""
				if n.Known {
					known = "★"
				}
				status := ""
				if n.Connected {
					status = "Connected"
				}
				signal := ""
				if n.Signal > 0 {
					signal = fmt.Sprintf("%d%%", n.Signal)
				}
				rows = append(rows, []string{n.SSID, known, status, wifitable.SecurityLabel(n.Security), signal})
			}
			fmt.Print(tui.RenderTable(headers, rows))
			return nil
		},
	}
}

// printNetworksJSON renders the extended schema required by the Linear issue.
func printNetworksJSON(networks []*agentpb.ListWiFiNetworksResponse_WiFiNetwork) error {
	type jsonNet struct {
		SSID        string `json:"ssid"`
		Security    string `json:"security"`
		IsKnown     bool   `json:"isKnown"`
		IsConnected bool   `json:"isConnected"`
		Signal      *int32 `json:"signal,omitempty"`
		Priority    *int32 `json:"priority,omitempty"`
		RssiDbm     *int32 `json:"rssiDbm,omitempty"`
	}
	out := make([]jsonNet, 0, len(networks))
	for _, n := range networks {
		out = append(out, jsonNet{
			SSID:        n.GetSsid(),
			Security:    wifitable.SecurityLabel(n.GetSecurity()),
			IsKnown:     n.GetIsKnown(),
			IsConnected: n.GetIsConnected(),
			Signal:      n.SignalStrength,
			Priority:    n.Priority,
			RssiDbm:     n.RssiDbm,
		})
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
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

			if ssid == "" {
				picked, pickErr := pickWifiNetwork(ctx, target)
				if pickErr != nil {
					return pickErr
				}
				ssid = picked
			}

			if !cmd.Flags().Changed("password") && term.IsTerminal(int(os.Stdin.Fd())) {
				if supportsKeychainLookup {
					fmt.Printf("Look up password for '%s' from keychain? (macOS will ask for permission) [Y/n] ", ssid)
					reader := bufio.NewReader(os.Stdin)
					line, _ := reader.ReadString('\n')
					answer := strings.TrimSpace(strings.ToLower(line))

					if answer == "" || answer == "y" || answer == "yes" {
						if kp, err := lookupKeychainPassword(ssid); err == nil && kp != "" {
							cliLogln("Using saved password from keychain.")
							password = kp
						} else {
							cliNotice("Password not available from keychain.")
						}
					}
				}

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

			if target.Bluetooth != nil && !target.Bluetooth.IsWendyAgent() {
				return wifiConnectViaBLELite(target.Bluetooth, ssid, password)
			}

			client, err := newWifiClient(target)
			if err != nil {
				return err
			}
			defer client.Close()

			if err := client.Connect(ctx, &agentpb.ConnectToWiFiRequest{
				Ssid:     ssid,
				Password: password,
			}); err != nil {
				return err
			}
			cliSuccess("Connected to %s", ssid)
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

			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				return wifiStatusViaBLEAgent(target.Bluetooth)
			}
			if target.Bluetooth != nil {
				return wifiStatusViaBLELite(target.Bluetooth)
			}
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
				cliSuccess("Connected to: %s", resp.GetSsid())
			} else {
				cliNotice("Not connected to any WiFi network.")
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

			if target.Bluetooth != nil && target.Bluetooth.IsWendyAgent() {
				return wifiDisconnectViaBLEAgent(target.Bluetooth)
			}
			if target.Bluetooth != nil {
				return wifiDisconnectViaBLELite(target.Bluetooth)
			}
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
			cliSuccess("Disconnected from WiFi.")
			return nil
		},
	}
}

func newWifiRankCmd() *cobra.Command {
	var ssid string
	var priority int
	var order string

	cmd := &cobra.Command{
		Use:   "rank",
		Short: "Set the autoconnect ranking of known WiFi networks",
		Long: `Set the priority of a single known network or bulk-reorder several.

Examples:
  wendy device wifi rank --ssid Home --priority 10
  wendy device wifi rank --order "Home,Office,Cafe"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			if order == "" && ssid == "" {
				return errors.New("must pass either --order or --ssid")
			}
			if order != "" && ssid != "" {
				return errors.New("--order and --ssid are mutually exclusive")
			}
			if ssid != "" && !cmd.Flags().Changed("priority") {
				return errors.New("--priority is required when --ssid is set")
			}

			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"))
			if err != nil {
				return err
			}
			defer target.Close()

			client, err := newWifiClient(target)
			if err != nil {
				return err
			}
			defer client.Close()

			if order != "" {
				var ssids []string
				for _, s := range strings.Split(order, ",") {
					s = strings.TrimSpace(s)
					if s != "" {
						ssids = append(ssids, s)
					}
				}
				if len(ssids) == 0 {
					return errors.New("--order must contain at least one SSID")
				}
				if err := client.Reorder(ctx, ssids); err != nil {
					return err
				}
				cliSuccess("Reordered %d known networks.", len(ssids))
				return nil
			}

			if err := client.SetPriority(ctx, ssid, int32(priority)); err != nil {
				return err
			}
			cliSuccess("Set %s priority to %d.", ssid, priority)
			return nil
		},
	}

	cmd.Flags().StringVar(&ssid, "ssid", "", "SSID of the known network to rank")
	cmd.Flags().IntVar(&priority, "priority", 0, "Autoconnect priority (higher = tried first)")
	cmd.Flags().StringVar(&order, "order", "", "Comma-separated list of SSIDs in priority order (highest first)")
	return cmd
}

func newWifiForgetCmd() *cobra.Command {
	var ssid string
	cmd := &cobra.Command{
		Use:   "forget",
		Short: "Remove a known WiFi network",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if ssid == "" {
				return errors.New("--ssid is required")
			}
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"))
			if err != nil {
				return err
			}
			defer target.Close()

			client, err := newWifiClient(target)
			if err != nil {
				return err
			}
			defer client.Close()

			if err := client.Forget(ctx, ssid); err != nil {
				return err
			}
			cliSuccess("Forgot %s.", ssid)
			return nil
		},
	}
	cmd.Flags().StringVar(&ssid, "ssid", "", "SSID of the known network to forget")
	return cmd
}

// ── WiFi network picker (legacy, still used by `connect`) ──────────

func pickWifiNetwork(ctx context.Context, target *SelectedDevice) (string, error) {
	type wifiEntry struct {
		ssid           string
		signalStrength int32
	}

	var networks []wifiEntry

	switch {
	case target.Bluetooth != nil && target.Bluetooth.IsWendyAgent():
		cliLogln("Scanning for WiFi networks on %s...", target.Bluetooth.DisplayName)
		tlsCfg, err := bleTLSConfig()
		if err != nil {
			return "", err
		}
		client, err := ble.ConnectAgent(target.Bluetooth, tlsCfg)
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
		cliLogln("Scanning for WiFi networks on this computer...")
		nets, err := scanLocalWifiNetworks()
		if err != nil {
			return "", fmt.Errorf("scanning local WiFi networks: %w", err)
		}
		for _, n := range nets {
			networks = append(networks, wifiEntry{ssid: n.SSID, signalStrength: n.SignalStrength})
		}

	case target.Agent != nil:
		cliLogln("Scanning for WiFi networks...")
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

// ── BLE WendyOS Agent / Lite helpers retained for status/disconnect ──

// bleTLSConfig loads the CLI certificate and builds a *tls.Config for mTLS
// over BLE L2CAP. Returns an error if the user is not logged in.
func bleTLSConfig() (*tls.Config, error) {
	auth := loadCLIAuth()
	if auth == nil || len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("not logged in; run 'wendy auth login' to authenticate")
	}
	cert := auth.Certificates[0]
	return ble.NewClientTLSConfig(cert.PemCertificate, cert.PemPrivateKey)
}

func wifiStatusViaBLEAgent(device *models.BluetoothDevice) error {
	cliLogln("Connecting to %s via Bluetooth...", device.DisplayName)
	tlsCfg, err := bleTLSConfig()
	if err != nil {
		return err
	}
	client, err := ble.ConnectAgent(device, tlsCfg)
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
		cliSuccess("Connected to: %s", resp.GetSsid())
	} else {
		cliNotice("Not connected to any WiFi network.")
	}
	return nil
}

func wifiDisconnectViaBLEAgent(device *models.BluetoothDevice) error {
	cliLogln("Connecting to %s via Bluetooth...", device.DisplayName)
	tlsCfg, err := bleTLSConfig()
	if err != nil {
		return err
	}
	client, err := ble.ConnectAgent(device, tlsCfg)
	if err != nil {
		return err
	}
	defer client.Close()
	if err := client.WifiDisconnect(); err != nil {
		return err
	}
	cliSuccess("Disconnected from WiFi.")
	return nil
}

// ── Local host WiFi scan (for Wendy Lite `list`) ──────────────────

func wifiListFromHost() error {
	cliLogln("Scanning for WiFi networks on this computer...")
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
		cliNotice("No WiFi networks found.")
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
	cliLogln("Connecting to %s via Bluetooth...", device.DisplayName)
	client, err := ble.ConnectLite(device)
	if err != nil {
		return err
	}
	defer client.Close()

	cliLogln("Provisioning WiFi '%s' on %s...", ssid, device.DisplayName)
	result, err := client.WifiConnect(ssid, password)
	if err != nil {
		return err
	}

	if result.Connected {
		if result.IPAddress != "" {
			cliSuccess("Connected to %s (IP: %s)", ssid, result.IPAddress)
		} else {
			cliSuccess("Connected to %s", ssid)
		}
	} else {
		return fmt.Errorf("failed to connect to %s", ssid)
	}
	return nil
}

func wifiStatusViaBLELite(device *models.BluetoothDevice) error {
	cliLogln("Connecting to %s via Bluetooth...", device.DisplayName)
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
			cliSuccess("Connected (IP: %s)", result.IPAddress)
		} else {
			cliSuccess("Connected to WiFi.")
		}
	} else {
		cliNotice("Not connected to any WiFi network.")
	}
	return nil
}

func wifiDisconnectViaBLELite(device *models.BluetoothDevice) error {
	cliLogln("Connecting to %s via Bluetooth...", device.DisplayName)
	client, err := ble.ConnectLite(device)
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.WifiClearCredentials(); err != nil {
		return fmt.Errorf("clearing WiFi credentials: %w", err)
	}

	cliSuccess("WiFi credentials cleared. Device will disconnect from WiFi.")
	return nil
}
