package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

func newDiscoverCmd() *cobra.Command {
	var discoverType string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Discover WendyOS devices on the network",
		Long:  "Continuously scan for WendyOS devices until Ctrl+C. Use --timeout to scan once for a fixed duration.",
		RunE: func(cmd *cobra.Command, args []string) error {
			opts := discovery.DiscoveryOptions{}

			switch discoverType {
			case "usb":
				opts.Types = []models.InterfaceType{models.InterfaceUSB}
			case "lan":
				opts.Types = []models.InterfaceType{models.InterfaceLAN}
			case "bluetooth":
				opts.Types = []models.InterfaceType{models.InterfaceBluetooth}
			case "external":
				opts.Types = []models.InterfaceType{models.InterfaceExternal}
			case "all", "":
				// discover all types
			default:
				return fmt.Errorf("unknown discovery type: %s (valid: usb, lan, bluetooth, external, all)", discoverType)
			}

			timeoutSet := cmd.Flags().Changed("timeout")

			if jsonOutput {
				if !timeoutSet {
					timeout = 5 * time.Second
				}
				opts.Timeout = timeout
				return discoverJSON(cmd.Context(), opts)
			}

			if timeoutSet {
				opts.Timeout = timeout
				return discoverOnce(cmd.Context(), opts)
			}
			return discoverContinuous(cmd.Context(), opts)
		},
	}

	cmd.Flags().StringVar(&discoverType, "type", "all", "Discovery type: usb, lan, bluetooth, external, all")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "Scan once for this duration then exit")

	return cmd
}

// discoverExternalDevices queries all registered providers for their devices.
// This uses AllProviders (not just available ones) so devices are discoverable
// even when the build toolchain isn't installed.
func discoverExternalDevices(ctx context.Context) []models.ExternalDevice {
	var all []models.ExternalDevice
	for _, p := range providers.AllProviders() {
		devices, err := p.DiscoverDevices(ctx)
		if err != nil {
			continue
		}
		all = append(all, devices...)
	}
	return all
}

// shouldIncludeExternal returns true if the discovery type filter includes external devices.
func shouldIncludeExternal(opts discovery.DiscoveryOptions) bool {
	if len(opts.Types) == 0 {
		return true // "all"
	}
	for _, t := range opts.Types {
		if t == models.InterfaceExternal {
			return true
		}
	}
	return false
}

func discoverJSON(ctx context.Context, opts discovery.DiscoveryOptions) error {
	collection, err := discovery.Discover(ctx, opts)
	if err != nil {
		return fmt.Errorf("discovery failed: %w", err)
	}

	collection.LANDevices = resolveLANVersions(ctx, collection.LANDevices)

	if shouldIncludeExternal(opts) {
		collection.ExternalDevices = discoverExternalDevices(ctx)
	}

	data, err := json.MarshalIndent(collection, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling results: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

// discoverOnce runs a single scan with the given timeout and prints results.
func discoverOnce(ctx context.Context, opts discovery.DiscoveryOptions) error {
	s := tui.NewSpinner("Scanning for WendyOS devices...")

	includeExternal := shouldIncludeExternal(opts)

	work := func() tea.Msg {
		collection, err := discovery.Discover(ctx, opts)
		if err == nil {
			collection.LANDevices = resolveLANVersions(ctx, collection.LANDevices)
			if includeExternal {
				collection.ExternalDevices = discoverExternalDevices(ctx)
			}
		}
		return tui.SpinnerDoneMsg{Result: collection, Err: err}
	}

	p := tea.NewProgram(s)
	go func() {
		p.Send(work())
	}()

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	model := finalModel.(tui.SpinnerModel)
	result, spinErr := model.Result()
	if spinErr != nil {
		return spinErr
	}

	collection, ok := result.(*models.DevicesCollection)
	if !ok || collection == nil || collection.IsEmpty() {
		fmt.Println("No devices found.")
		return nil
	}

	fmt.Print(renderDeviceTable(collection))
	return nil
}

// discoverContinuous runs scans in a loop, refreshing the table until Ctrl+C.
func discoverContinuous(ctx context.Context, opts discovery.DiscoveryOptions) error {
	opts.Timeout = 3 * time.Second // per-scan timeout
	m := newDiscoverModel(ctx, opts)
	p := tea.NewProgram(m)
	if _, err := p.Run(); err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	return nil
}

// --- Bubble Tea model for continuous discovery ---
// Each discovery type (USB, Ethernet, LAN, Bluetooth, External) runs as an
// independent tea.Cmd so results stream in as soon as each completes.

type usbScanMsg struct{ devices []models.USBDevice }
type ethScanMsg struct{ devices []models.EthernetInterface }
type lanScanMsg struct{ devices []models.LANDevice }
type btScanMsg struct{ devices []models.BluetoothDevice }
type extScanMsg struct{ devices []models.ExternalDevice }

type discoverModel struct {
	ctx             context.Context
	opts            discovery.DiscoveryOptions
	collection      *models.DevicesCollection
	quitting        bool
	hasResults      bool
	err             error
	includeExternal bool
}

func newDiscoverModel(ctx context.Context, opts discovery.DiscoveryOptions) discoverModel {
	return discoverModel{
		ctx:             ctx,
		opts:            opts,
		collection:      &models.DevicesCollection{},
		includeExternal: shouldIncludeExternal(opts),
	}
}

func (m discoverModel) shouldDiscover(t models.InterfaceType) bool {
	if len(m.opts.Types) == 0 {
		return true
	}
	for _, ot := range m.opts.Types {
		if ot == t {
			return true
		}
	}
	return false
}

func (m discoverModel) scanUSB() tea.Cmd {
	return func() tea.Msg {
		devices, _ := discovery.DiscoverUSB(m.ctx)
		return usbScanMsg{devices: devices}
	}
}

func (m discoverModel) scanEthernet() tea.Cmd {
	return func() tea.Msg {
		devices, _ := discovery.DiscoverEthernet(m.ctx)
		return ethScanMsg{devices: devices}
	}
}

func (m discoverModel) scanLAN() tea.Cmd {
	return func() tea.Msg {
		devices, _ := discovery.DiscoverLAN(m.ctx, m.opts.Timeout)
		devices = resolveLANVersions(m.ctx, devices)
		return lanScanMsg{devices: devices}
	}
}

func (m discoverModel) scanBluetooth() tea.Cmd {
	return func() tea.Msg {
		activeScan := len(m.opts.Types) == 0 || len(m.opts.Types) == 1
		devices, _ := discovery.DiscoverBluetooth(m.ctx, activeScan)
		return btScanMsg{devices: devices}
	}
}

func (m discoverModel) scanExternal() tea.Cmd {
	return func() tea.Msg {
		return extScanMsg{devices: discoverExternalDevices(m.ctx)}
	}
}

func (m discoverModel) Init() tea.Cmd {
	var cmds []tea.Cmd
	if m.shouldDiscover(models.InterfaceUSB) {
		cmds = append(cmds, m.scanUSB())
	}
	if m.shouldDiscover(models.InterfaceEthernet) {
		cmds = append(cmds, m.scanEthernet())
	}
	if m.shouldDiscover(models.InterfaceLAN) {
		cmds = append(cmds, m.scanLAN())
	}
	if m.shouldDiscover(models.InterfaceBluetooth) {
		cmds = append(cmds, m.scanBluetooth())
	}
	if m.includeExternal {
		cmds = append(cmds, m.scanExternal())
	}
	return tea.Batch(cmds...)
}

func (m discoverModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}
	case usbScanMsg:
		m.collection.USBDevices = msg.devices
		m.hasResults = true
		return m, delayThen(usbPollInterval, m.scanUSB())
	case ethScanMsg:
		m.collection.EthernetInterfaces = msg.devices
		m.hasResults = true
		return m, delayThen(ethernetPollInterval, m.scanEthernet())
	case lanScanMsg:
		m.collection.LANDevices = msg.devices
		m.hasResults = true
		return m, m.scanLAN()
	case btScanMsg:
		m.collection.BluetoothDevices = msg.devices
		m.hasResults = true
		return m, m.scanBluetooth()
	case extScanMsg:
		m.collection.ExternalDevices = msg.devices
		m.hasResults = true
		return m, delayThen(externalPollInterval, m.scanExternal())
	}

	return m, nil
}

func parseDurationEnv(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func delayThen(d time.Duration, cmd tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		if d > 0 {
			time.Sleep(d)
		}
		return cmd()
	}
}

var (
	usbPollInterval      = parseDurationEnv("WENDY_DISCOVER_USB_INTERVAL", 3*time.Second)
	ethernetPollInterval = parseDurationEnv("WENDY_DISCOVER_ETHERNET_INTERVAL", 3*time.Second)
	externalPollInterval = parseDurationEnv("WENDY_DISCOVER_EXTERNAL_INTERVAL", 5*time.Second)
)

var (
	dimStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	scanStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
)

func (m discoverModel) View() string {
	if m.quitting {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(scanStyle.Render("⟳ Scanning for WendyOS devices...") + dimStyle.Render(" (press q or Ctrl+C to stop)") + "\n\n")

	if m.err != nil {
		sb.WriteString(fmt.Sprintf("Error: %v\n", m.err))
	}

	if !m.collection.IsEmpty() {
		sb.WriteString(renderDeviceTable(m.collection))
	} else if m.hasResults {
		sb.WriteString(dimStyle.Render("No devices found yet...") + "\n")
	}

	return sb.String()
}

// --- shared table rendering ---

func renderDeviceTable(collection *models.DevicesCollection) string {
	headers := []string{"Name", "Type", "Address", "Port", "Version"}
	var rows [][]string

	for _, d := range collection.USBDevices {
		rows = append(rows, []string{d.DisplayName, "USB", d.Hostname, "", d.AgentVersion})
	}
	for _, d := range collection.MergedDevices() {
		port := ""
		if d.Port() > 0 {
			port = fmt.Sprintf("%d", d.Port())
		}
		rows = append(rows, []string{d.DisplayName, d.ConnectionTypes(), d.Address(), port, d.AgentVersion})
	}
	for _, d := range collection.EthernetInterfaces {
		rows = append(rows, []string{d.DisplayName, "Ethernet", d.IPAddress, "", d.AgentVersion})
	}
	for _, d := range collection.ExternalDevices {
		// Wendy Lite devices are merged with BLE Lite in MergedDevices().
		if d.ProviderKey == "wendy-lite" {
			continue
		}
		addr := fmt.Sprintf("%s: %s", d.ProviderKey, d.ID)
		typeName := d.ProviderKey
		if p := providers.ProviderForKey(d.ProviderKey); p != nil {
			typeName = p.DisplayName()
		}
		rows = append(rows, []string{d.DisplayName, typeName, addr, "", d.AgentVersion})
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i][1] != rows[j][1] {
			return rows[i][1] < rows[j][1]
		}
		return strings.ToLower(rows[i][0]) < strings.ToLower(rows[j][0])
	})

	return tui.RenderTable(headers, rows)
}
