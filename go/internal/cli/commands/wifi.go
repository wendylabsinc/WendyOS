package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/ble"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui/wifitable"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"golang.org/x/term"
)

var (
	resolveWifiTargetFn = resolveTarget
	pickWifiNetworkFn   = pickWifiNetwork
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
		newWifiRegulatoryCmd(),
	)

	return cmd
}

// ── Interactive TUI entry point ─────────────────────────────────────

func runWifiInteractive(cmd *cobra.Command) error {
	ctx := cmd.Context()
	target, err := resolveWifiTargetFn(ctx, ExcludeProviders("local", "docker"), IncludeBluetooth())
	if err != nil {
		return err
	}
	defer target.Close()

	if target.Bluetooth != nil && !target.Bluetooth.IsWendyAgent() {
		return runWifiConnect(cmd, target, "", "")
	}
	if target.Provider != nil && target.External != nil {
		if _, ok := target.Provider.(providers.WifiManager); ok {
			return runWifiConnect(cmd, target, "", "")
		}
	}

	client, err := newWifiClient(target)
	if err != nil {
		return err
	}
	defer client.Close()

	// Start with an empty table: the model's Init fires the first refresh and
	// keeps polling, so the TUI opens instantly and rows stream in while the
	// device-side rescan (which can take several seconds) fills its cache.
	model := wifitable.NewModel(nil).WithHandler(&wifiTUIHandler{ctx: ctx, client: client})
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
		client, err := connectBLEAgent(target.Bluetooth)
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
			if macErr := macOSBetaUnsupportedFeatureError(ctx, c.agent, err, "Wi-Fi network scanning"); macErr != nil {
				return nil, fmt.Errorf("listing WiFi networks: %w", macErr)
			}
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
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"), IncludeBluetooth())
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
			target, err := resolveWifiTargetFn(ctx, ExcludeProviders("local", "docker"), IncludeBluetooth())
			if err != nil {
				return err
			}
			defer target.Close()

			return runWifiConnect(cmd, target, ssid, password)
		},
	}

	cmd.Flags().StringVar(&ssid, "ssid", "", "WiFi network SSID")
	cmd.Flags().StringVar(&password, "password", "", "WiFi network password")

	return cmd
}

// runWifiConnect reuses the selected target for explicit connect commands and
// the default WiFi command on Wendy Lite.
func runWifiConnect(cmd *cobra.Command, target *SelectedDevice, ssid, password string) error {
	ctx := cmd.Context()
	if ssid == "" {
		picked, pickErr := pickWifiNetworkFn(ctx, target)
		if pickErr != nil {
			return pickErr
		}
		ssid = picked
	}

	if !cmd.Flags().Changed("password") && term.IsTerminal(int(os.Stdin.Fd())) {
		if supportsKeychainLookup {
			if confirmFn(fmt.Sprintf("Look up password for '%s' from keychain? (macOS will ask for permission)", ssid)) {
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

	if target.Provider != nil && target.External != nil {
		if wm, ok := target.Provider.(providers.WifiManager); ok {
			return wm.WifiConnect(ctx, *target.External, ssid, password)
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
}

func newWifiStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Get current WiFi connection status",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"), IncludeBluetooth())
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
				if macErr := macOSBetaUnsupportedFeatureError(ctx, target.Agent.AgentService, err, "Wi-Fi status reporting"); macErr != nil {
					return fmt.Errorf("getting WiFi status: %w", macErr)
				}
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
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"), IncludeBluetooth())
			if err != nil {
				return err
			}
			defer target.Close()

			if target.Provider != nil && target.External != nil {
				if wm, ok := target.Provider.(providers.WifiManager); ok {
					return wm.WifiDisconnect(ctx, *target.External)
				}
			}
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

			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"), IncludeBluetooth())
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
			target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"), IncludeBluetooth())
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

// ── WiFi network picker (used by `connect`) ────────────────────────

// wifiPickerColumns renders PickerItems built by wifiPickerItems /
// localWifiPickerItems: Name carries the SSID, Type the security label and
// Size the signal percentage.
func wifiPickerColumns() []tui.PickerColumn {
	return []tui.PickerColumn{
		{Title: "SSID", MinWidth: 24, Required: true, Value: func(it tui.PickerItem) string { return it.Name }},
		{Title: "Security", MinWidth: 10, Value: func(it tui.PickerItem) string { return it.Type }},
		{Title: "Signal", MinWidth: 8, Value: func(it tui.PickerItem) string { return it.Size }},
	}
}

func wifiPickerItems(networks []*agentpb.ListWiFiNetworksResponse_WiFiNetwork) []tui.PickerItem {
	items := make([]tui.PickerItem, 0, len(networks))
	for _, n := range networks {
		signal := ""
		if n.GetSignalStrength() > 0 {
			signal = fmt.Sprintf("%d%%", n.GetSignalStrength())
		}
		// SSIDs come from over-the-air beacon frames; strip control
		// characters so hostile names can't inject escape sequences.
		ssid := tui.StripControl(n.GetSsid())
		items = append(items, tui.PickerItem{
			Name:  ssid,
			Type:  wifitable.SecurityLabel(n.GetSecurity()),
			Size:  signal,
			Value: ssid,
		})
	}
	return items
}

func localWifiPickerItems(networks []localWifiNetwork) []tui.PickerItem {
	items := make([]tui.PickerItem, 0, len(networks))
	for _, n := range networks {
		signal := ""
		if n.SignalStrength > 0 {
			signal = fmt.Sprintf("%d%%", n.SignalStrength)
		}
		items = append(items, tui.PickerItem{
			Name:  n.SSID,
			Type:  tui.StripControl(n.Security),
			Size:  signal,
			Value: n.SSID,
		})
	}
	return items
}

// pickWifiNetwork shows the network picker immediately and feeds scan results
// in as they arrive, instead of blocking on the scan before any UI appears.
// Typing filters the list; matches are highlighted in the SSID column.
func pickWifiNetwork(ctx context.Context, target *SelectedDevice) (string, error) {
	picker := tui.NewPickerWithTitleAndColumns("Select a WiFi network", wifiPickerColumns())
	picker.Filterable = true
	p := tea.NewProgram(picker)

	scanCtx, cancelScan := context.WithCancel(ctx)
	defer cancelScan()

	var scanErrMu sync.Mutex
	var scanErr error
	recordScanErr := func(err error) {
		scanErrMu.Lock()
		defer scanErrMu.Unlock()
		if scanErr == nil {
			scanErr = err
		}
	}

	// WifiManager providers cannot scan networks on the device itself (yet), so
	// their devices are offered the networks visible from the host machine.
	_, providerManagesWifi := target.Provider.(providers.WifiManager)

	switch {
	case (target.Bluetooth != nil && !target.Bluetooth.IsWendyAgent()) || providerManagesWifi:
		// Wendy Lite (over BLE or a WifiManager provider) — scan from the host
		// machine. Cached results paint the picker instantly, then the fresh
		// rescan fills it in, so SSIDs trickle in rather than appearing all at
		// once when the scan completes.
		go func() {
			defer p.Send(tui.PickerDoneMsg{})
			if err := streamLocalWifiScan(scanCtx, func(batch []localWifiNetwork) {
				p.Send(tui.PickerAddMsg{Items: localWifiPickerItems(batch)})
			}); err != nil {
				recordScanErr(fmt.Errorf("scanning local WiFi networks: %w", err))
			}
		}()

	case target.Bluetooth != nil || target.Agent != nil:
		client, err := newWifiClient(target)
		if err != nil {
			return "", err
		}
		defer client.Close()

		// Authoritative scan: blocks until the device-side rescan finishes.
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer p.Send(tui.PickerDoneMsg{})
			nets, err := client.List(scanCtx)
			if err != nil {
				recordScanErr(err)
				return
			}
			p.Send(tui.PickerAddMsg{Items: wifiPickerItems(nets)})
		}()

		// While the rescan runs, poll the device's scan cache so SSIDs stream
		// into the picker as they are found. Each poll returns quickly: the
		// in-progress scan rejects new rescan requests and the list call reads
		// the cache. Only over gRPC — the BLE transport can't multiplex
		// concurrent calls. The deadline caps RPC traffic if the
		// authoritative scan wedges; its result still lands whenever it
		// finishes.
		if client.agent != nil {
			go func() {
				ticker := time.NewTicker(1500 * time.Millisecond)
				defer ticker.Stop()
				deadline := time.After(30 * time.Second)
				lastSeen := ""
				for {
					select {
					case <-done:
						return
					case <-scanCtx.Done():
						return
					case <-deadline:
						return
					case <-ticker.C:
						nets, err := client.List(scanCtx)
						if err != nil {
							// Surfaced after the picker exits if nothing was
							// ever found; transient poll errors are expected
							// while the device scan is busy.
							recordScanErr(err)
							continue
						}
						items := wifiPickerItems(nets)
						// Skip redraws when the cache hasn't changed.
						sig := make([]string, 0, len(items))
						for _, it := range items {
							sig = append(sig, it.Name)
						}
						if joined := strings.Join(sig, "\x00"); joined != lastSeen {
							lastSeen = joined
							p.Send(tui.PickerAddMsg{Items: items})
						}
					}
				}
			}()
		}

	default:
		return "", fmt.Errorf("selected device does not support WiFi network scanning")
	}

	finalModel, err := p.Run()
	cancelScan()
	if err != nil {
		return "", fmt.Errorf("network picker: %w", err)
	}

	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return "", ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		scanErrMu.Lock()
		err := scanErr
		scanErrMu.Unlock()
		if err != nil {
			return "", err
		}
		if wifiScanCacheHint != "" {
			return "", fmt.Errorf("no WiFi networks found (%s)", wifiScanCacheHint)
		}
		return "", fmt.Errorf("no WiFi networks found")
	}

	ssid, ok := sel.Value.(string)
	if !ok {
		return "", fmt.Errorf("invalid picker selection")
	}
	return ssid, nil
}

// ── BLE WendyOS Agent / Lite helpers retained for status/disconnect ──

func wifiStatusViaBLEAgent(device *models.BluetoothDevice) error {
	cliLogln("Connecting to %s via Bluetooth...", device.DisplayName)
	client, err := connectBLEAgent(device)
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
	client, err := connectBLEAgent(device)
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
	networks, err := scanLocalWifiNetworks(context.Background())
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
		if wifiScanCacheHint != "" {
			cliNotice("No WiFi networks found. %s.", wifiScanCacheHint)
		} else {
			cliNotice("No WiFi networks found.")
		}
		return nil
	}

	headers := []string{"SSID", "Security", "Signal"}
	var rows [][]string
	for _, n := range networks {
		signal := ""
		if n.SignalStrength > 0 {
			signal = fmt.Sprintf("%d%%", n.SignalStrength)
		}
		// Scanner output is already sanitized at ingestion; strip again at
		// the render boundary so this site is safe in isolation.
		rows = append(rows, []string{tui.StripControl(n.SSID), tui.StripControl(n.Security), signal})
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
