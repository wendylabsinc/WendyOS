package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/env"
	"github.com/wendylabsinc/wendy/internal/shared/models"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
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
	annotateLANUSBFromEthernet(collection)
	sortLANDevicesForDiscover(collection.LANDevices)

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
			annotateLANUSBFromEthernet(collection)
			sortLANDevicesForDiscover(collection.LANDevices)
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
type btScanMsg struct {
	devices []models.BluetoothDevice
	err     error
}
type extScanMsg struct{ devices []models.ExternalDevice }

// discoverDeviceInfo is the JSON structure copied to the clipboard.
type discoverDeviceInfo struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	USB     string `json:"usb,omitempty"`
	Address string `json:"address"`
	Version string `json:"version,omitempty"`
}

// flashClearMsg is sent after a delay to clear the flash message.
type flashClearMsg struct{}

// discoverUpdateDoneMsg is sent when a background device update completes.
type discoverUpdateDoneMsg struct {
	deviceName string
	assetID    int32
	err        error
}

// bleRetentionPeriod is how long a BLE device stays visible after it was last
// seen in a scan. BLE scans are lossy (devices may miss a scan cycle even when
// in range), so we keep results around longer than a single scan window.
const bleRetentionPeriod = 30 * time.Second

type discoverModel struct {
	ctx                context.Context
	opts               discovery.DiscoveryOptions
	collection         *models.DevicesCollection
	bleSeen            map[string]time.Time // device ID -> time last seen in a BLE scan
	usbInterval        increasingRefreshInterval
	ethernetInterval   increasingRefreshInterval
	externalInterval   increasingRefreshInterval
	table              bubbleTable.Model
	quitting           bool
	hasResults         bool
	err                error
	includeExternal    bool
	windowHeight       int
	bleWarning         string
	flashMessage       string
	flashIsError       bool
	updatingDeviceName string // non-empty while a background update is running
}

func newDiscoverModel(ctx context.Context, opts discovery.DiscoveryOptions) discoverModel {
	m := discoverModel{
		ctx:             ctx,
		opts:            opts,
		collection:      &models.DevicesCollection{},
		bleSeen:         make(map[string]time.Time),
		table:           newDiscoverTable(true),
		includeExternal: shouldIncludeExternal(opts),
	}
	m.refreshTable()
	return m
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
		sortLANDevicesForDiscover(devices)
		return lanScanMsg{devices: devices}
	}
}

func (m discoverModel) scanBluetooth() tea.Cmd {
	return func() tea.Msg {
		activeScan := len(m.opts.Types) == 0 || len(m.opts.Types) == 1
		devices, err := discovery.DiscoverBluetooth(m.ctx, activeScan)
		return btScanMsg{devices: devices, err: err}
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
	case tea.WindowSizeMsg:
		m.windowHeight = msg.Height
		m.refreshTable()
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			rows := discoverTableRows(m.collection)
			cursor := m.table.Cursor()
			if len(rows) > 0 && cursor >= 0 && cursor < len(rows) {
				row := rows[cursor]
				info := deviceInfoFromRow(row)
				m.flashMessage, m.flashIsError = copyDeviceJSON(info)
				return m, clearFlashAfter(5 * time.Second)
			}
			return m, nil
		case "a":
			rows := discoverTableRows(m.collection)
			if len(rows) > 0 {
				var all []discoverDeviceInfo
				for _, row := range rows {
					all = append(all, deviceInfoFromRow(row))
				}
				m.flashMessage, m.flashIsError = copyDeviceJSON(all)
				if !m.flashIsError {
					m.flashMessage = "Copied all devices as JSON to clipboard."
				}
				return m, clearFlashAfter(5 * time.Second)
			}
			return m, nil
		case "u":
			if m.updatingDeviceName != "" {
				return m, nil // already updating
			}
			rows := discoverTableRows(m.collection)
			cursor := m.table.Cursor()
			if len(rows) == 0 || cursor < 0 || cursor >= len(rows) {
				return m, nil
			}
			row := rows[cursor]
			addr := lanDeviceAddr(m.collection, row[rowNameIndex])
			if addr == "" {
				m.flashMessage = "Update is only supported for LAN devices."
				m.flashIsError = true
				return m, clearFlashAfter(3 * time.Second)
			}
			rowVer := strings.TrimPrefix(row[rowVersionIndex], "* ")
			if rowVer == "" || version.CompareVersions(version.Version, rowVer) <= 0 {
				m.flashMessage = "Device is already up to date."
				m.flashIsError = false
				return m, clearFlashAfter(3 * time.Second)
			}
			m.updatingDeviceName = row[rowNameIndex]
			m.flashMessage = "Updating " + row[rowNameIndex] + "..."
			m.flashIsError = false
			return m, m.startDeviceUpdateCmd(addr, row[rowNameIndex])
		case "d":
			rows := discoverTableRows(m.collection)
			cursor := m.table.Cursor()
			if len(rows) > 0 && cursor >= 0 && cursor < len(rows) {
				// Use the display name as the device identifier — for LAN devices
				// this is the mDNS hostname which resolveDeviceAddress can resolve.
				deviceID := rows[cursor][rowNameIndex]
				// For LAN devices, prefer the address column (hostname.local).
				addr := rows[cursor][rowAddressIndex]
				if addr != "" && !strings.Contains(addr, ":") {
					deviceID = addr
				} else if host, _, err := net.SplitHostPort(addr); err == nil && host != "" {
					deviceID = host
				}
				if cfg, err := config.Load(); err == nil {
					cfg.DefaultDevice = deviceID
					_ = config.Save(cfg)
				}
				m.flashMessage = "Default device set to: " + deviceID
				m.flashIsError = false
				m.refreshTable()
				return m, clearFlashAfter(3 * time.Second)
			}
			return m, nil
		case "x":
			if cfg, err := config.Load(); err == nil {
				cfg.DefaultDevice = ""
				_ = config.Save(cfg)
			}
			m.flashMessage = "Default device cleared."
			m.flashIsError = false
			m.refreshTable()
			return m, clearFlashAfter(3 * time.Second)
		}
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd
	case usbScanMsg:
		m.collection.USBDevices = msg.devices
		m.hasResults = true
		m.refreshTable()
		delay := m.usbInterval.delay(env.DiscoverUSBInterval())
		return m, delayThen(delay, m.scanUSB())
	case ethScanMsg:
		m.collection.EthernetInterfaces = msg.devices
		m.hasResults = true
		m.refreshTable()
		delay := m.ethernetInterval.delay(env.DiscoverEthernetInterval())
		return m, delayThen(delay, m.scanEthernet())
	case lanScanMsg:
		// Preserve last known AgentVersion and DeviceType when the gRPC probe
		// failed. The probe uses a 1500 ms timeout, so transient latency can
		// cause a blank for one scan cycle even though the device is still up.
		for i := range msg.devices {
			if msg.devices[i].AgentVersion != "" {
				continue
			}
			for _, prev := range m.collection.LANDevices {
				if strings.EqualFold(prev.DisplayName, msg.devices[i].DisplayName) && prev.AgentVersion != "" {
					msg.devices[i].AgentVersion = prev.AgentVersion
					msg.devices[i].DeviceType = prev.DeviceType
					break
				}
			}
		}
		m.collection.LANDevices = msg.devices
		m.hasResults = true
		m.refreshTable()
		return m, m.scanLAN()
	case btScanMsg:
		now := time.Now()

		// Update last-seen timestamps for everything in this scan.
		for _, dev := range msg.devices {
			key := dev.ID
			if key == "" {
				key = dev.DisplayName
			}
			m.bleSeen[key] = now
		}

		// Build a merged list: fresh scan results (authoritative) plus any
		// previously-seen devices that are still within the retention window.
		inNewScan := make(map[string]bool, len(msg.devices))
		merged := make([]models.BluetoothDevice, 0, len(msg.devices))
		for _, dev := range msg.devices {
			merged = append(merged, dev)
			key := dev.ID
			if key == "" {
				key = dev.DisplayName
			}
			inNewScan[key] = true
		}
		for _, existing := range m.collection.BluetoothDevices {
			key := existing.ID
			if key == "" {
				key = existing.DisplayName
			}
			if !inNewScan[key] {
				if lastSeen, ok := m.bleSeen[key]; ok && now.Sub(lastSeen) < bleRetentionPeriod {
					merged = append(merged, existing)
				}
			}
		}

		m.collection.BluetoothDevices = merged
		m.hasResults = true
		m.refreshTable()
		if msg.err != nil {
			m.bleWarning = msg.err.Error()
			return m, nil // stop retrying BLE scans when unavailable
		}
		m.bleWarning = ""
		return m, m.scanBluetooth()
	case extScanMsg:
		m.collection.ExternalDevices = msg.devices
		m.hasResults = true
		m.refreshTable()
		delay := m.externalInterval.delay(env.DiscoverExternalInterval())
		return m, delayThen(delay, m.scanExternal())
	case flashClearMsg:
		m.flashMessage = ""
		m.flashIsError = false
	case discoverUpdateDoneMsg:
		m.updatingDeviceName = ""
		if msg.err != nil {
			m.flashMessage = fmt.Sprintf("Update failed for %s: %v", msg.deviceName, msg.err)
			m.flashIsError = true
		} else {
			m.flashMessage = fmt.Sprintf("Updated %s successfully.", msg.deviceName)
			m.flashIsError = false
		}
		return m, clearFlashAfter(10 * time.Second)
	}

	return m, nil
}

func delayThen(d time.Duration, cmd tea.Cmd) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(d)
		return cmd()
	}
}

var (
	dimStyle        = lipgloss.NewStyle().Foreground(tui.ColorDim)
	scanStyle       = lipgloss.NewStyle().Foreground(tui.ColorPrimary)
	flashStyle      = lipgloss.NewStyle().Foreground(tui.ColorAccent)
	flashErrorStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196")) // red
)

func (m discoverModel) View() string {
	if m.quitting {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(scanStyle.Render("⟳ Scanning for WendyOS devices...") + "\n")
	if m.updatingDeviceName != "" {
		sb.WriteString(dimStyle.Render("  updating "+m.updatingDeviceName+"... (q quit)") + "\n")
	} else {
		sb.WriteString(dimStyle.Render("  ↑/↓ navigate, enter copy, a copy all, u update, d set default, x unset default, q quit") + "\n")
	}

	if m.bleWarning != "" {
		sb.WriteString(dimStyle.Render("  Bluetooth: "+m.bleWarning) + "\n")
	}

	sb.WriteString("\n")

	if m.err != nil {
		sb.WriteString(fmt.Sprintf("Error: %v\n", m.err))
	}

	if !m.collection.IsEmpty() {
		sb.WriteString(m.table.View() + "\n")
	} else if m.hasResults {
		sb.WriteString(dimStyle.Render("No devices found yet...") + "\n")
	}

	if m.flashMessage != "" {
		style := flashStyle
		if m.flashIsError {
			style = flashErrorStyle
		} else if m.updatingDeviceName != "" {
			style = scanStyle
		}
		sb.WriteString("\n" + style.Render("  "+m.flashMessage) + "\n")
	}

	return sb.String()
}

func (m *discoverModel) refreshTable() {
	rows := discoverTableRows(m.collection)
	m.table.SetColumns(discoverTableColumns(rows))
	m.table.SetRows(rows)
	if len(rows) > 0 && m.table.Cursor() < 0 {
		m.table.SetCursor(0)
	}
	m.table.SetWidth(discoverTableWidth(m.table.Columns()))
	m.table.SetHeight(discoverTableHeight(len(rows), m.windowHeight, true))
}

// markOutdated prefixes the version string with "* " when the agent is behind
// the CLI, serving as a visible indicator in the discover table.
func markOutdated(agentVer string) string {
	if agentVer != "" && version.CompareVersions(version.Version, agentVer) > 0 {
		return "* " + agentVer
	}
	return agentVer
}

// lanDeviceAddr returns the gRPC address for the first LAN device whose
// DisplayName matches (case-insensitive). Returns "" if not found.
func lanDeviceAddr(collection *models.DevicesCollection, displayName string) string {
	for i := range collection.LANDevices {
		d := &collection.LANDevices[i]
		if strings.EqualFold(d.DisplayName, displayName) {
			return preferredLANAddress(*d)
		}
	}
	return ""
}

// startDeviceUpdateCmd returns a Bubble Tea command that connects to addr,
// downloads and uploads the latest agent binary, and waits for the device to
// restart. It sends a discoverUpdateDoneMsg when finished.
func (m discoverModel) startDeviceUpdateCmd(addr, name string) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		conn, err := connectWithAutoTLS(ctx, addr)
		if err != nil {
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("connecting to device: %w", err)}
		}
		versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("querying device: %w", err)}
		}
		arch := versionResp.GetCpuArchitecture()
		if arch == "" {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("device did not report CPU architecture")}
		}

		release, err := fetchAgentRelease(false)
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("fetching release: %w", err)}
		}

		assetPrefix := fmt.Sprintf("wendy-agent-linux-%s-", arch)
		var matchedAsset *githubReleaseAsset
		for _, a := range release.Assets {
			if strings.HasPrefix(a.Name, assetPrefix) && strings.HasSuffix(a.Name, ".tar.gz") {
				matchedAsset = &a
				break
			}
		}
		if matchedAsset == nil {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("no asset for linux/%s in release %s", arch, release.TagName)}
		}

		binaryData, err := downloadAgentBinary(*matchedAsset)
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("downloading binary: %w", err)}
		}

		h := sha256.Sum256(binaryData)
		sha256Hash := hex.EncodeToString(h[:])

		if err := deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash); err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("uploading: %w", err)}
		}
		conn.Close() // agent is restarting

		newConn, err := waitForAgentRestart(ctx, addr)
		if err != nil {
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("waiting for restart: %w", err)}
		}
		newConn.Close()
		return discoverUpdateDoneMsg{deviceName: name}
	}
}

// --- shared table rendering ---

func renderDeviceTable(collection *models.DevicesCollection) string {
	rows := discoverTableRows(collection)
	if len(rows) == 0 {
		return ""
	}

	t := newDiscoverTable(false)
	t.SetColumns(discoverTableColumns(rows))
	t.SetRows(rows)
	t.SetWidth(discoverTableWidth(t.Columns()))
	t.SetHeight(discoverTableHeight(len(rows), 0, false))

	return t.View() + "\n"
}

var (
	discoverTableHeaders   = []string{"", "Name", "Device Type", "USB", "Address", "Version"}
	discoverTableMinWidths = []int{3, 12, 10, 5, 14, 10}
	discoverTableMaxWidths = []int{3, 33, 20, 24, 28, 16}
)

const (
	rowDefaultIndex = iota
	rowNameIndex
	rowDeviceTypeIndex
	rowUSBIndex
	rowAddressIndex
	rowVersionIndex
)

func newDiscoverTable(interactive bool) bubbleTable.Model {
	return tui.NewBubbleTable(interactive, discoverTableColumns(nil))
}

var deviceTypeNames = map[string]string{
	"raspberry-pi-3":   "Raspberry Pi 3",
	"raspberry-pi-4":   "Raspberry Pi 4",
	"raspberry-pi-5":   "Raspberry Pi 5",
	"jetson-agx-orin":  "Jetson AGX Orin",
	"jetson-orin-nano": "Jetson Orin Nano",
	"x86_64":           "x86-64",
}

func humanReadableDeviceType(dt string) string {
	if name, ok := deviceTypeNames[dt]; ok {
		return name
	}
	return dt
}

func sortLANDevicesForDiscover(devices []models.LANDevice) {
	sort.SliceStable(devices, func(i, j int) bool {
		iHasUSB := devices[i].USB != ""
		jHasUSB := devices[j].USB != ""
		if iHasUSB != jHasUSB {
			return iHasUSB
		}
		return strings.ToLower(devices[i].DisplayName) < strings.ToLower(devices[j].DisplayName)
	})
}

func annotateLANUSBFromEthernet(collection *models.DevicesCollection) {
	if collection == nil || len(collection.EthernetInterfaces) == 0 {
		return
	}

	byInterfaceName := make(map[string]models.EthernetInterface, len(collection.EthernetInterfaces))
	for _, iface := range collection.EthernetInterfaces {
		if iface.Name == "" {
			continue
		}
		byInterfaceName[strings.ToLower(iface.Name)] = iface
	}

	for i := range collection.LANDevices {
		dev := &collection.LANDevices[i]
		if dev.USB != "" {
			continue
		}
		interfaceName := dev.NetworkInterface
		if interfaceName == "" {
			interfaceName = interfaceNameFromScopedAddress(dev.IPAddress)
		}
		if interfaceName == "" {
			continue
		}
		if iface, ok := byInterfaceName[strings.ToLower(interfaceName)]; ok {
			dev.USB = ethernetInterfaceUSBSummary(iface)
		}
	}
}

func interfaceNameFromScopedAddress(addr string) string {
	_, zone, ok := strings.Cut(addr, "%")
	if !ok {
		return ""
	}
	return zone
}

func ethernetInterfaceUSBSummary(iface models.EthernetInterface) string {
	label := iface.Name
	if iface.DisplayName != "" && !strings.EqualFold(iface.DisplayName, iface.Name) {
		label = fmt.Sprintf("%s (%s)", iface.DisplayName, iface.Name)
	}
	if iface.LinkSpeed != "" {
		return label + " " + iface.LinkSpeed
	}
	return label
}

func discoverTableRows(collection *models.DevicesCollection) []bubbleTable.Row {
	var rows []bubbleTable.Row
	annotateLANUSBFromEthernet(collection)

	// Load default device to show ★ indicator.
	var defaultDevice string
	if cfg, err := config.Load(); err == nil {
		defaultDevice = strings.ToLower(cfg.DefaultDevice)
	}

	defaultMark := func(name string) string {
		if defaultDevice != "" && strings.ToLower(name) == defaultDevice {
			return "★"
		}
		return ""
	}

	for _, d := range collection.USBDevices {
		deviceType := ""
		if d.IsESP32 {
			deviceType = "ESP32"
		}
		rows = append(rows, bubbleTable.Row{defaultMark(d.DisplayName), d.DisplayName, deviceType, d.USBVersion, d.Hostname, markOutdated(d.AgentVersion)})
	}
	for _, d := range collection.MergedDevices() {
		deviceType := ""
		usb := ""
		if d.LAN != nil && d.LAN.DeviceType != "" {
			deviceType = humanReadableDeviceType(d.LAN.DeviceType)
		} else if d.Bluetooth != nil && !d.Bluetooth.IsWendyAgent() {
			deviceType = "ESP32"
		}
		if d.LAN != nil {
			usb = d.LAN.USB
		}
		rows = append(rows, bubbleTable.Row{defaultMark(d.DisplayName), d.DisplayName, deviceType, usb, d.Address(), markOutdated(d.AgentVersion)})
	}
	for _, d := range collection.EthernetInterfaces {
		rows = append(rows, bubbleTable.Row{defaultMark(d.DisplayName), d.DisplayName, "", "", d.IPAddress, markOutdated(d.AgentVersion)})
	}
	for _, d := range collection.ExternalDevices {
		// Wendy Lite devices are merged with BLE Lite in MergedDevices().
		if d.ProviderKey == "wendy-lite" {
			continue
		}
		addr := fmt.Sprintf("%s: %s", d.ProviderKey, d.ID)
		rows = append(rows, bubbleTable.Row{defaultMark(d.DisplayName), d.DisplayName, "", "", addr, markOutdated(d.AgentVersion)})
	}

	sort.Slice(rows, func(i, j int) bool {
		iHasUSB := rows[i][rowUSBIndex] != ""
		jHasUSB := rows[j][rowUSBIndex] != ""
		if iHasUSB != jHasUSB {
			return iHasUSB
		}
		if rows[i][rowDeviceTypeIndex] != rows[j][rowDeviceTypeIndex] {
			return rows[i][rowDeviceTypeIndex] < rows[j][rowDeviceTypeIndex]
		}
		return strings.ToLower(rows[i][rowNameIndex]) < strings.ToLower(rows[j][rowNameIndex])
	})

	return rows
}

func discoverTableColumns(rows []bubbleTable.Row) []bubbleTable.Column {
	cols := make([]bubbleTable.Column, len(discoverTableHeaders))
	for i, title := range discoverTableHeaders {
		width := lipgloss.Width(title)
		for _, row := range rows {
			if i >= len(row) {
				continue
			}
			width = max(width, lipgloss.Width(row[i]))
		}
		width += 2
		width = max(width, discoverTableMinWidths[i])
		width = min(width, discoverTableMaxWidths[i])
		cols[i] = bubbleTable.Column{Title: title, Width: width}
	}
	return cols
}

func discoverTableWidth(cols []bubbleTable.Column) int {
	total := 0
	for _, col := range cols {
		total += col.Width + 2
	}
	return total
}

func discoverTableHeight(rowCount, windowHeight int, interactive bool) int {
	height := rowCount + 1
	if !interactive {
		return max(height, 1)
	}

	height = max(height, 4)
	if windowHeight > 0 {
		return min(height, max(windowHeight-4, 4))
	}
	return min(height, 12)
}

// deviceInfoFromRow converts a table row to a discoverDeviceInfo.
func deviceInfoFromRow(row bubbleTable.Row) discoverDeviceInfo {
	return discoverDeviceInfo{
		Name:    row[rowNameIndex],
		Type:    row[rowDeviceTypeIndex],
		USB:     row[rowUSBIndex],
		Address: row[rowAddressIndex],
		Version: strings.TrimPrefix(row[rowVersionIndex], "* "),
	}
}

// copyDeviceJSON marshals v as indented JSON, copies it to the clipboard,
// and returns a user-facing message and whether it's an error.
func copyDeviceJSON(v interface{}) (message string, isError bool) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("Copy failed: %v", err), true
	}
	if err := clipboardWriter(string(data)); err != nil {
		return fmt.Sprintf("Copy failed: %v", err), true
	}
	return "Copied device info as JSON to clipboard.", false
}

// clearFlashAfter returns a tea.Cmd that sends flashClearMsg after the given duration.
func clearFlashAfter(d time.Duration) tea.Cmd {
	return func() tea.Msg {
		time.Sleep(d)
		return flashClearMsg{}
	}
}

// clipboardWriter is the function used to copy text to the clipboard.
// It is a package-level variable so tests can replace it.
var clipboardWriter = copyToClipboard

// clipboardCandidate describes a clipboard tool and its arguments.
type clipboardCandidate struct {
	name string
	args []string
}

// execLookPath and execCommand are package-level variables so tests can stub them.
var execLookPath = exec.LookPath
var execCommand = exec.Command

func shouldCaptureClipboardStderr(goos string) bool {
	// wl-copy, xclip, and xsel commonly fork into the background to keep owning
	// the clipboard selection. If os/exec captures stderr through a pipe, the
	// daemonized child can inherit that pipe and Cmd.Wait can block indefinitely.
	return goos != "linux"
}

func runClipboardCommand(cmd *exec.Cmd, timeout time.Duration) error {
	cmd.WaitDelay = 500 * time.Millisecond
	if err := cmd.Start(); err != nil {
		return err
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case err := <-done:
		return err
	case <-timer.C:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case <-done:
		case <-time.After(cmd.WaitDelay + 100*time.Millisecond):
		}
		return fmt.Errorf("timed out after %s", timeout)
	}
}

// clipboardCommandTimeout bounds clipboard helper execution. Some Linux clipboard
// tools daemonize to own the selection; if they (or their child processes) keep
// inherited file descriptors open, waiting on the command can otherwise hang the
// interactive discover TUI.
var clipboardCommandTimeout = 2 * time.Second

// copyToClipboard writes text to the system clipboard using platform tools.
func copyToClipboard(text string) error {
	var candidates []clipboardCandidate
	switch runtime.GOOS {
	case "darwin":
		candidates = []clipboardCandidate{
			{name: "pbcopy"},
		}
	case "linux":
		candidates = []clipboardCandidate{
			{name: "wl-copy"},
			{name: "xclip", args: []string{"-selection", "clipboard"}},
			{name: "xsel", args: []string{"--clipboard", "--input"}},
		}
	case "windows":
		candidates = []clipboardCandidate{
			{name: "clip.exe"},
		}
	default:
		return fmt.Errorf("clipboard not supported on %s; copy the output manually", runtime.GOOS)
	}
	var errs []string
	for _, c := range candidates {
		if _, err := execLookPath(c.name); err != nil {
			continue
		}
		var stderr bytes.Buffer
		cmd := execCommand(c.name, c.args...)
		cmd.Stdin = strings.NewReader(text)
		if shouldCaptureClipboardStderr(runtime.GOOS) {
			cmd.Stderr = &stderr
		}
		if err := runClipboardCommand(cmd, clipboardCommandTimeout); err != nil {
			detail := stderr.String()
			if detail == "" {
				detail = err.Error()
			}
			errs = append(errs, fmt.Sprintf("%s: %s", c.name, strings.TrimSpace(detail)))
			continue
		}
		return nil
	}
	if len(errs) > 0 {
		return fmt.Errorf("all clipboard tools failed: %s", strings.Join(errs, "; "))
	}
	names := make([]string, len(candidates))
	for i, c := range candidates {
		names[i] = c.name
	}
	return fmt.Errorf("no clipboard tool found; install one of: %s", strings.Join(names, ", "))
}
