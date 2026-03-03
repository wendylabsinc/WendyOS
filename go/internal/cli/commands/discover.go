package commands

import (
	"context"
	"encoding/json"
	"fmt"
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

// discoverExternalDevices queries all available providers for their devices.
func discoverExternalDevices(ctx context.Context) []models.ExternalDevice {
	var all []models.ExternalDevice
	for _, p := range providers.AvailableProviders() {
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
		if err == nil && includeExternal {
			collection.ExternalDevices = discoverExternalDevices(ctx)
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

type scanResultMsg struct {
	collection *models.DevicesCollection
	err        error
}

type discoverModel struct {
	ctx             context.Context
	opts            discovery.DiscoveryOptions
	collection      *models.DevicesCollection
	scanning        bool
	scanCount       int
	quitting        bool
	err             error
	includeExternal bool
}

func newDiscoverModel(ctx context.Context, opts discovery.DiscoveryOptions) discoverModel {
	return discoverModel{
		ctx:             ctx,
		opts:            opts,
		includeExternal: shouldIncludeExternal(opts),
	}
}

func (m discoverModel) startScan() tea.Cmd {
	return func() tea.Msg {
		collection, err := discovery.Discover(m.ctx, m.opts)
		if err == nil && m.includeExternal {
			collection.ExternalDevices = discoverExternalDevices(m.ctx)
		}
		return scanResultMsg{collection: collection, err: err}
	}
}

func (m discoverModel) Init() tea.Cmd {
	return m.startScan()
}

func (m discoverModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		}

	case scanResultMsg:
		m.scanCount++
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.collection = msg.collection
		}
		return m, m.startScan()
	}

	return m, nil
}

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

	if m.collection != nil && !m.collection.IsEmpty() {
		sb.WriteString(renderDeviceTable(m.collection))
	} else if m.scanCount > 0 {
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
	for _, d := range collection.LANDevices {
		addr := d.Hostname
		if d.IPAddress != "" {
			addr = d.IPAddress
		}
		port := ""
		if d.Port > 0 {
			port = fmt.Sprintf("%d", d.Port)
		}
		rows = append(rows, []string{d.DisplayName, "LAN", addr, port, d.AgentVersion})
	}
	for _, d := range collection.BluetoothDevices {
		rows = append(rows, []string{d.DisplayName, "Bluetooth", d.Address, "", d.AgentVersion})
	}
	for _, d := range collection.EthernetInterfaces {
		rows = append(rows, []string{d.DisplayName, "Ethernet", d.IPAddress, "", d.AgentVersion})
	}
	for _, d := range collection.ExternalDevices {
		addr := fmt.Sprintf("%s: %s", d.ProviderKey, d.ID)
		rows = append(rows, []string{d.DisplayName, "External", addr, "", d.AgentVersion})
	}

	return tui.RenderTable(headers, rows)
}
