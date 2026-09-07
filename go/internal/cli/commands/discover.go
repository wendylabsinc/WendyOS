package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func newDiscoverCmd() *cobra.Command {
	var discoverType string
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "discover",
		Short: "Discover local and cloud WendyOS devices",
		Long:  "Continuously discover WendyOS devices in Local and Cloud tabs until Ctrl+C. Use --timeout to scan local devices once for a fixed duration.",
		RunE: func(cmd *cobra.Command, args []string) error {
			var opts discovery.DiscoveryOptions

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

			// Pre-flight: on Linux, if a USB-C Wendy device is tethered but its
			// host link isn't configured, offer to set it up so it shows up in
			// the scan below. No-op on other platforms / non-interactive runs.
			_ = maybeOfferUSBSetup(cmd.Context())

			timeoutSet := cmd.Flags().Changed("timeout")

			// Only the JSON and one-shot paths read opts.LAN; the continuous
			// TUI streams LAN devices itself (startLANStream). Both want the
			// CLI's cache+probe collection, and building it here rather than
			// up front spares the TUI a second simulator filter, whose
			// learners would dial every booting VM twice.
			if jsonOutput {
				if !timeoutSet {
					timeout = 5 * time.Second
				}
				opts.Timeout = timeout
				opts.LAN = cliLANStreamOptions(cmd.Context())
				// JSON output always lists every target so scripts/MCP keep the
				// full set regardless of WENDY_SHOW_LOCAL_DEVICES.
				return discoverJSON(cmd.Context(), opts)
			}

			if timeoutSet {
				opts.Timeout = timeout
				opts.LAN = cliLANStreamOptions(cmd.Context())
				return discoverOnce(cmd.Context(), opts, providers.ShowLocalDevices())
			}
			return discoverContinuous(cmd.Context(), opts, providers.ShowLocalDevices())
		},
	}

	cmd.Flags().StringVar(&discoverType, "type", "all", "Local discovery type: usb, lan, bluetooth, external, all")
	cmd.Flags().DurationVar(&timeout, "timeout", 5*time.Second, "Scan once for this duration then exit")

	return cmd
}

// discoverExternalDevices queries registered providers for their devices. This
// uses AllProviders (not just available ones) so devices are discoverable even
// when the build toolchain isn't installed. Unless includeLocal is set (see
// providers.ShowLocalDevices), local run targets (this machine, Docker/OrbStack,
// Apple Container) are skipped so the table lists separate WendyOS devices by
// default.
func discoverExternalDevices(ctx context.Context, includeLocal bool) []models.ExternalDevice {
	var all []models.ExternalDevice
	for _, p := range providers.AllProviders() {
		if !includeLocal && providers.IsLocalProviderKey(p.Key()) {
			continue
		}
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
	collection, err := discoverLocalTargets(ctx, opts)
	if err != nil {
		return fmt.Errorf("discovery failed: %w", err)
	}

	annotateLANUSBFromEthernet(collection)
	sortLANDevicesForDiscover(collection.LANDevices)

	if shouldIncludeExternal(opts) {
		// JSON output always includes local run targets (see newDiscoverCmd).
		collection.ExternalDevices = discoverExternalDevices(ctx, true)
	}

	data, err := json.MarshalIndent(collection, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling results: %w", err)
	}
	fmt.Println(string(data))
	return nil
}

// discoverOnce runs a single scan with the given timeout and prints results.
// includeLocal surfaces local run targets that are hidden by default.
func discoverOnce(ctx context.Context, opts discovery.DiscoveryOptions, includeLocal bool) error {
	s := tui.NewSpinner("Scanning for WendyOS devices...")

	includeExternal := shouldIncludeExternal(opts)

	work := func() tea.Msg {
		collection, err := discoverLocalTargets(ctx, opts)
		if err == nil {
			annotateLANUSBFromEthernet(collection)
			sortLANDevicesForDiscover(collection.LANDevices)
			if includeExternal {
				collection.ExternalDevices = discoverExternalDevices(ctx, includeLocal)
			}
		}
		return tui.SpinnerDoneMsg{Result: collection, Err: err}
	}

	p := tui.NewProgressProgram(s)
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
		fmt.Println(noDevicesHint())
		return nil
	}

	fmt.Print(renderDeviceTable(collection))
	return nil
}

// noDevicesHint returns short, OS-appropriate guidance shown when discovery
// finds nothing. mDNS discovery needs multicast on the path; on Linux the
// browse uses Avahi (or raw multicast) and is commonly defeated by a stopped
// avahi-daemon or a firewall blocking UDP 5353.
func noDevicesHint() string {
	if runtime.GOOS == "linux" {
		return "Hints:\n" +
			"  • Is avahi-daemon running?   systemctl status avahi-daemon\n" +
			"  • Firewall blocking mDNS?    sudo ufw allow 5353/udp   (if ufw is active)\n" +
			"  • USB-C tethered device?     re-run 'wendy discover' and accept the USB setup prompt\n" +
			"  • Or connect directly by IP: wendy device connect <ip>:50051"
	}
	return "Hints:\n" +
		"  • Device powered on and on the same network (or USB-C tethered).\n" +
		"  • mDNS uses UDP 5353 — make sure a firewall isn't blocking it.\n" +
		"  • Or connect directly by IP: wendy device connect <ip>:50051"
}

// discoverContinuous runs scans in a loop, refreshing the table until Ctrl+C.
// includeLocal surfaces local run targets that are hidden by default.
func discoverContinuous(ctx context.Context, opts discovery.DiscoveryOptions, includeLocal bool) error {
	cfg, err := config.Load()
	if err != nil {
		cfg = nil
	}
	cloudAuth := devicePickerInitialAuth(cfg)
	var createReq *errCreateSimulator
	// Which tab to open on. Only a create changes it, so the user lands back
	// where they pressed the key rather than on Local.
	openOn := devicePickerLocalTab
	for {
		err := discoverContinuousWithCloudAuth(ctx, opts, includeLocal, cloudAuth, defaultOrgForCloudAuth(cfg, cloudAuth), openOn)
		openOn = devicePickerLocalTab
		switch {
		case errors.Is(err, errDevicePickerLogin):
			if err := performLogin(ctx, defaultCloudDashboard, defaultCloudGRPC); err != nil {
				return err
			}
			cfg, err = config.Load()
			if err != nil {
				return fmt.Errorf("loading config after login: %w", err)
			}
			cloudAuth = devicePickerInitialAuth(cfg)
		case errors.As(err, &createReq):
			// The TUI is gone by now, so the prompt and the download's own
			// progress program have the terminal to themselves. Same helper the
			// run picker uses, so "c" does the same thing in both views.
			name := createReq.name
			createReq = nil
			if createErr := createSimulator(name); createErr != nil {
				if errors.Is(createErr, ErrUserCancelled) {
					openOn = devicePickerSimulatorTab
					continue
				}
				return createErr
			}
			openOn = devicePickerSimulatorTab
		case errors.Is(err, errDevicePickerSwitchOrg):
			cfg, err = config.Load()
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			picked, freshCfg, pickErr := switchCloudOrganization(ctx, cfg)
			if errors.Is(pickErr, ErrUserCancelled) {
				continue
			}
			if pickErr != nil {
				return pickErr
			}
			cloudAuth = picked
			cfg = freshCfg
		default:
			return err
		}
	}
}

func defaultOrgForCloudAuth(cfg *config.Config, auth *config.AuthConfig) int32 {
	if cfg == nil {
		return 0
	}
	if cfg.DefaultOrgID != 0 {
		return cfg.DefaultOrgID
	}
	if auth != nil && cfg.DefaultCloudGRPC == auth.CloudGRPC {
		return cloudAuthOrgID(auth)
	}
	return 0
}

func discoverContinuousWithCloudAuth(ctx context.Context, opts discovery.DiscoveryOptions, includeLocal bool, cloudAuth *config.AuthConfig, defaultOrg int32, openOn devicePickerTab) error {
	opts.Timeout = 3 * time.Second // per-scan timeout
	discoverCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	local := newDiscoverModel(discoverCtx, opts, includeLocal)
	m := newDiscoverTabsModel(discoverCtx, local, cloudAuth, defaultOrg, openOn)
	// Alt screen: the table grows to fill the window, and the alternate
	// buffer restores the user's terminal content when the TUI exits.
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}
	dm, ok := finalModel.(discoverTabsModel)
	if !ok {
		return fmt.Errorf("discover TUI returned unexpected model %T", finalModel)
	}
	switch dm.action {
	case devicePickerLogin:
		return errDevicePickerLogin
	case devicePickerSwitchOrg:
		return errDevicePickerSwitchOrg
	case devicePickerCreateVM:
		return &errCreateSimulator{name: dm.createVMName}
	}
	return nil
}

// --- Bubble Tea model for continuous discovery ---
// Each discovery type (USB, Ethernet, LAN, Bluetooth, External) runs as an
// independent tea.Cmd so results stream in as soon as each completes.

type usbScanMsg struct{ devices []models.USBDevice }
type ethScanMsg struct{ devices []models.EthernetInterface }
type btScanMsg struct {
	devices []models.BluetoothDevice
	err     error
}
type extScanMsg struct{ devices []models.ExternalDevice }

// lanEventMsg carries one event off the LAN discovery stream (see
// discovery.StreamLAN), plus the channel it came from so Update can re-arm
// waitLANEvent and keep draining it.
type lanEventMsg struct {
	ev discovery.LANEvent
	ch <-chan discovery.LANEvent
}

// waitLANEvent returns a tea.Cmd that blocks for the next event on ch. A
// closed channel (the stream session ended) yields a nil message, which
// bubbletea silently drops — Update does not re-arm in that case.
func waitLANEvent(ch <-chan discovery.LANEvent) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return lanEventMsg{ev: ev, ch: ch}
	}
}

// discoverDeviceInfo is the JSON structure copied to the clipboard.
type discoverDeviceInfo struct {
	ID          int32  `json:"id,omitempty"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	USB         string `json:"usb,omitempty"`
	Address     string `json:"address"`
	Version     string `json:"version,omitempty"`
	Provisioned string `json:"provisioned,omitempty"`
}

type discoverTableItem struct {
	picker  tui.PickerItem
	info    discoverDeviceInfo
	lanName string
	// lanKey is discoverycache.Key(ID, DisplayName) for this row's LAN
	// device — the same identity upsertLANDevice merges rows by. It is the
	// map key into m.probe, kept distinct from lanName (which stays a plain
	// display name for the 'u'-key update lookup in lanDeviceAddr) so a
	// DisplayName change or a DisplayName collision between two different
	// device IDs can never orphan or cross-wire a probe state. Empty for
	// non-LAN rows.
	lanKey        string
	defaultDevice string
}

// flashClearMsg is sent after a delay to clear the flash message.
type flashClearMsg struct{}

// discoverUpdateDoneMsg is sent when a background device update completes.
type discoverUpdateDoneMsg struct {
	deviceName string
	assetID    int32
	err        error
	// note carries a non-error outcome that is not a successful update, e.g.
	// the device already running the release we would install.
	note string
	// version is the probe taken while deciding, set only alongside note so a
	// caller can refresh a cached row it would otherwise leave stale.
	version *agentpb.GetAgentVersionResponse
}

// bleRetentionPeriod is how long a BLE device stays visible after it was last
// seen in a scan. BLE scans are lossy (devices may miss a scan cycle even when
// in range), so we keep results around longer than a single scan window.
const bleRetentionPeriod = 30 * time.Second

type discoverModel struct {
	ctx                context.Context
	opts               discovery.DiscoveryOptions
	collection         *models.DevicesCollection
	tableItems         []discoverTableItem  // cached by refreshTable; row order matches the table
	bleSeen            map[string]time.Time // device ID -> time last seen in a BLE scan
	usbInterval        increasingRefreshInterval
	ethernetInterval   increasingRefreshInterval
	externalInterval   increasingRefreshInterval
	table              tui.BubbleTable
	quitting           bool
	hasResults         bool
	err                error
	includeExternal    bool
	windowWidth        int
	windowHeight       int
	bleWarning         string
	flashMessage       string
	flashIsError       bool
	updatingDeviceName string                    // non-empty while a background update is running
	spinner            spinner.Model             // animates Agent/OS cells while LAN probes run
	probe              map[string]tui.ProbeState // LAN display name (lowercased) -> probe state
	includeLocal       bool                      // surface local run targets hidden by default
}

func newDiscoverModel(ctx context.Context, opts discovery.DiscoveryOptions, includeLocal bool) discoverModel {
	m := discoverModel{
		ctx:             ctx,
		opts:            opts,
		collection:      &models.DevicesCollection{},
		bleSeen:         make(map[string]time.Time),
		table:           newDiscoverTable(true),
		includeExternal: shouldIncludeExternal(opts),
		includeLocal:    includeLocal,
		// Empty style so View() yields a bare frame rune for the plain table
		// cells (matches tui.newProbeSpinner).
		spinner: spinner.New(spinner.WithSpinner(spinner.Dot)),
		probe:   make(map[string]tui.ProbeState),
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

// startLANStream begins a StreamLAN session and returns the tea.Cmd that
// waits for its first event. The engine emits cached rows instantly (so a
// device seen in a prior run appears with no delay), then live sightings and
// probe outcomes, and handles its own offline detection and retry — this
// model just mirrors whatever it reports. Prober must be set: with a nil
// Prober a cached row can never be confirmed offline.
func (m discoverModel) startLANStream() tea.Cmd {
	events := lanStreamFn(m.ctx, cliLANStreamOptions(m.ctx))
	return waitLANEvent(events)
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
		return extScanMsg{devices: discoverExternalDevices(m.ctx, m.includeLocal)}
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
		cmds = append(cmds, m.startLANStream())
	}
	if m.shouldDiscover(models.InterfaceBluetooth) {
		cmds = append(cmds, m.scanBluetooth())
	}
	if m.includeExternal {
		cmds = append(cmds, m.scanExternal())
	}
	cmds = append(cmds, m.spinner.Tick)
	return tea.Batch(cmds...)
}

func (m discoverModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.windowWidth = msg.Width
		m.windowHeight = msg.Height
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		m.refreshTable()
		return m, cmd
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			items := m.tableItems
			cursor := m.table.Cursor()
			if len(items) > 0 && cursor >= 0 && cursor < len(items) {
				m.flashMessage, m.flashIsError = copyDeviceJSON(items[cursor].info)
				return m, clearFlashAfter(5 * time.Second)
			}
			return m, nil
		case "a":
			items := m.tableItems
			if len(items) > 0 {
				var all []discoverDeviceInfo
				for _, item := range items {
					all = append(all, item.info)
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
			items := m.tableItems
			cursor := m.table.Cursor()
			if len(items) == 0 || cursor < 0 || cursor >= len(items) {
				return m, nil
			}
			item := items[cursor]
			addr := lanDeviceAddr(m.collection, item.lanName)
			if addr == "" {
				m.flashMessage = "Update is only supported for LAN devices."
				m.flashIsError = true
				return m, clearFlashAfter(3 * time.Second)
			}
			// Whether an update is needed is decided against the release
			// channel in startDeviceUpdateCmd, not against this CLI's version.
			m.updatingDeviceName = item.info.Name
			m.flashMessage = "Checking " + item.info.Name + "..."
			m.flashIsError = false
			return m, m.startDeviceUpdateCmd(addr, item.info.Name)
		case "d":
			items := m.tableItems
			cursor := m.table.Cursor()
			if len(items) > 0 && cursor >= 0 && cursor < len(items) {
				deviceID := items[cursor].defaultDevice
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
	case lanEventMsg:
		if msg.ev.Kind == discovery.LANRetracted {
			// Listed, then found to be one of this machine's VMs: it belongs
			// on the Simulator tab, not here.
			m.removeLANDevice(discoverycache.Key(msg.ev.Device.ID, msg.ev.Device.DisplayName))
			m.refreshTable()
			return m, waitLANEvent(msg.ch)
		}
		// A superseded identity is this same device under a stale,
		// connect-minted hostname key; dropping it keeps one row per device.
		if msg.ev.Supersedes != "" {
			m.removeLANDevice(msg.ev.Supersedes)
		}
		m.upsertLANDevice(msg.ev.Device)
		// Keyed by the same identity upsertLANDevice merges rows by (TXT
		// device ID, falling back to display name) — not the display name
		// alone, which can collide across devices or change out from under a
		// single device, either of which would orphan/cross-wire this state.
		key := discoverycache.Key(msg.ev.Device.ID, msg.ev.Device.DisplayName)
		m.probe[key], _ = lanRowState(msg.ev)
		m.hasResults = true
		m.refreshTable()
		return m, waitLANEvent(msg.ch)
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		if m.anyProbePending() {
			m.refreshTable()
		}
		return m, cmd
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
		m.flashMessage, m.flashIsError = discoverUpdateFlash(msg)
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
	hintWarnStyle   = lipgloss.NewStyle().Foreground(tui.ColorNotice)
)

func (m discoverModel) View() string {
	if m.quitting {
		return ""
	}

	var sb strings.Builder

	sb.WriteString(m.viewLine(scanStyle.Render("⟳ Scanning for WendyOS devices...")) + "\n")
	if m.updatingDeviceName != "" {
		// One keypress covers the channel check and the upload it may not reach,
		// so the wording has to fit both.
		sb.WriteString(m.viewLine(dimStyle.Render("  working on "+m.updatingDeviceName+"... (q quit)")) + "\n")
	} else {
		scrollHint := ""
		if m.canScrollTable() {
			scrollHint = ", ←/→ scroll"
		}
		sb.WriteString(m.viewLine(dimStyle.Render("  ↑/↓ navigate"+scrollHint+", enter copy, a copy all, u update, d set default, x unset default, q quit")) + "\n")
	}

	if m.bleWarning != "" {
		sb.WriteString(m.viewLine(dimStyle.Render("  Bluetooth: "+m.bleWarning)) + "\n")
	}

	sb.WriteString("\n")

	if m.err != nil {
		sb.WriteString(m.viewLine(fmt.Sprintf("Error: %v", m.err)) + "\n")
	}

	if !m.collection.IsEmpty() {
		sb.WriteString(tui.ColorizeProbeGlyphs(m.tableView()) + "\n")
		sb.WriteString(m.viewLine(dimStyle.Render("  "+tui.DeviceTableLegend(discoverPickerItems(m.tableItems)))) + "\n")
		if hint := m.selectedHint(); hint != "" {
			sb.WriteString(m.viewLine(hintWarnStyle.Render("  ⚠  "+hint)) + "\n")
		}
	} else if m.hasResults {
		sb.WriteString(m.viewLine(dimStyle.Render("No devices found yet...")) + "\n")
	}

	if m.flashMessage != "" {
		style := flashStyle
		if m.flashIsError {
			style = flashErrorStyle
		} else if m.updatingDeviceName != "" {
			style = scanStyle
		}
		sb.WriteString("\n" + m.viewLine(style.Render("  "+m.flashMessage)) + "\n")
	}

	return sb.String()
}

// anyProbePending reports whether any LAN device still has a probe in flight.
func (m discoverModel) anyProbePending() bool {
	for _, st := range m.probe {
		if st == tui.ProbePending {
			return true
		}
	}
	return false
}

// upsertLANDevice merges dev into m.collection.LANDevices, keyed by
// discoverycache.Key(dev.ID, dev.DisplayName) — the same identity the on-disk
// cache uses. A device already present (e.g. a LANCached row confirmed by a
// later LANFound/LANUpdated) is replaced in place so the row count never
// grows for the same physical device; an unseen device is appended. Offline
// devices are merged like any other event: the row stays listed, never
// removed, per the picker's "never drop a known device" contract.
func (m *discoverModel) upsertLANDevice(dev models.LANDevice) {
	key := discoverycache.Key(dev.ID, dev.DisplayName)
	for i := range m.collection.LANDevices {
		if discoverycache.Key(m.collection.LANDevices[i].ID, m.collection.LANDevices[i].DisplayName) == key {
			m.collection.LANDevices[i] = dev
			sortLANDevicesForDiscover(m.collection.LANDevices)
			return
		}
	}
	m.collection.LANDevices = append(m.collection.LANDevices, dev)
	sortLANDevicesForDiscover(m.collection.LANDevices)
}

// removeLANDevice drops the row for a cache identity the discovery engine has
// superseded — a connect-minted, hostname-derived row replaced by the same
// device's real TXT identity (discovery.LANEvent.Supersedes) — so one physical
// device never occupies two rows.
func (m *discoverModel) removeLANDevice(key string) {
	for i := range m.collection.LANDevices {
		if discoverycache.Key(m.collection.LANDevices[i].ID, m.collection.LANDevices[i].DisplayName) == key {
			m.collection.LANDevices = append(m.collection.LANDevices[:i], m.collection.LANDevices[i+1:]...)
			break
		}
	}
	delete(m.probe, key)
}

func (m *discoverModel) refreshTable() {
	m.tableItems = discoverTableItems(m.collection)
	// Stamp each LAN row with its probe state (and the current spinner frame
	// while connecting) so the Agent/OS columns animate / show the error glyph.
	frame := m.spinner.View()
	for i := range m.tableItems {
		if m.tableItems[i].lanName == "" {
			continue
		}
		st := m.probe[m.tableItems[i].lanKey]
		m.tableItems[i].picker.Probe = st
		if st == tui.ProbePending {
			m.tableItems[i].picker.ProbeFrame = frame
			// Still connecting: don't show the no-access hint yet.
			m.tableItems[i].picker.Hint = ""
		}
	}
	pickerItems := discoverPickerItems(m.tableItems)
	cols, rows := tui.PickerDeviceTableData(pickerItems, discoverDefaultKey(), true)
	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	if len(rows) > 0 && m.table.Cursor() < 0 {
		m.table.SetCursor(0)
	}
	m.table.SetWidth(tui.PickerTableWidth(m.table.Columns()))
	m.table.SetHeight(tui.PickerTableHeight(len(rows), m.windowHeight))
}

// selectedHint returns the hint for the highlighted table row, e.g. the
// no-access explanation for a provisioned device this CLI cannot query.
func (m discoverModel) selectedHint() string {
	cursor := m.table.Cursor()
	if cursor < 0 || cursor >= len(m.tableItems) {
		return ""
	}
	return strings.TrimSpace(m.tableItems[cursor].picker.Hint)
}

func (m discoverModel) viewLine(line string) string {
	if m.windowWidth <= 0 {
		return line
	}
	return tui.CropANSIView(line, 0, m.windowWidth)
}

func (m discoverModel) tableView() string {
	return m.table.View()
}

func (m discoverModel) canScrollTable() bool {
	return m.table.CanScroll()
}

func (m discoverModel) tableViewportWidth() int {
	if width := m.table.ViewportWidth(); width > 0 {
		return width
	}
	return tui.PickerTableWidth(m.table.Columns())
}

func lanDeviceAddr(collection *models.DevicesCollection, displayName string) string {
	for i := range collection.LANDevices {
		d := &collection.LANDevices[i]
		if strings.EqualFold(d.DisplayName, displayName) {
			return preferredLANAddress(*d)
		}
	}
	return ""
}

func (m discoverModel) startDeviceUpdateCmd(addr, name string) tea.Cmd {
	ctx := m.ctx
	return func() tea.Msg {
		latestVer, _, err := resolveAgentVersion(false)
		if err != nil {
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("resolving agent version: %w", err)}
		}
		conn, err := connectWithAutoTLS(ctx, addr)
		if err != nil {
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("connecting to device: %w", err)}
		}
		versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("querying device: %w", err)}
		}
		if note := agentAlreadyAtReleaseNote(name, versionResp.GetVersion(), latestVer); note != "" {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, note: note}
		}
		arch := versionResp.GetCpuArchitecture()
		if arch == "" {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("device did not report CPU architecture")}
		}
		osName := versionResp.GetOs()

		binaryData, actualVer, _, err := resolveAgentArtifact(osName, arch, false)
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: fmt.Errorf("resolving agent binary: %w", err)}
		}
		if err := checkDarwinArtifactVersion(osName, latestVer, actualVer); err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{deviceName: name, err: err}
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
	items := discoverTableItems(collection)
	pickerItems := discoverPickerItems(items)
	cols, rows := tui.PickerDeviceTableData(pickerItems, discoverDefaultKey(), true)
	if len(rows) == 0 {
		return ""
	}

	t := newDiscoverTable(false)
	t.SetColumns(cols)
	t.SetRows(rows)
	t.SetWidth(tui.PickerTableWidth(t.Columns()))
	t.SetHeight(max(len(rows)+1, 1))

	return t.View() + "\n" + dimStyle.Render("  "+tui.DeviceTableLegend(pickerItems)) + "\n"
}

func newDiscoverTable(interactive bool) tui.BubbleTable {
	return tui.NewBubbleTable(interactive, nil)
}

// These back the `wendy cloud discover` table. The Address column is omitted:
// cloud devices are addressed by name/ID via the broker tunnel, so the IP adds
// noise. (The interactive `wendy discover` table uses tui.PickerDeviceTableData,
// not these.) The full address is still available in the clipboard JSON.
var (
	discoverTableHeaders   = []string{"", "Name", "Type", "Version"}
	discoverTableMinWidths = []int{3, 12, 10, 10}
	discoverTableMaxWidths = []int{3, 33, 20, 16}
)

var deviceTypeNames = map[string]string{
	"raspberry-pi-3":   "Raspberry Pi 3",
	"raspberry-pi-4":   "Raspberry Pi 4",
	"raspberry-pi-5":   "Raspberry Pi 5",
	"jetson-agx-orin":  "Jetson AGX Orin",
	"jetson-orin-nano": "Jetson Orin Nano",
	"jetson-agx-thor":  "Jetson AGX Thor",
	"x86_64":           "x86-64",
	"vm-arm64":         "ARM64 VM",
	"vm-x86-64":        "x86-64 VM",
}

func humanReadableDeviceType(dt string) string {
	if name, ok := deviceTypeNames[dt]; ok {
		return name
	}
	return dt
}

// humanReadableOSType builds a Type label from an os/architecture pair for
// machines with no board-specific device type (e.g. Mac agents, generic
// Linux hosts), falling back to whatever information is present.
func humanReadableOSType(os, arch string) string {
	osRaw := strings.TrimSpace(os)
	osNorm := strings.ToLower(osRaw)
	arch = humanReadableArch(arch)

	var label string
	switch osNorm {
	case "":
		return ""
	case "darwin", "macos":
		label = "macOS"
	case "windows":
		label = "Windows"
	case "wendyos", "edgeos":
		label = "WendyOS"
	case "linux", "ubuntu", "debian", "alpine", "raspbian":
		label = "Linux"
	default:
		label = osRaw
	}
	if arch != "" {
		return fmt.Sprintf("%s (%s)", label, arch)
	}
	return label
}

func humanReadableArch(arch string) string {
	arch = strings.ToLower(strings.TrimSpace(arch))
	switch arch {
	case "amd64", "x86_64":
		return "x86_64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return arch
	}
}

// discoverNoAccessHint explains a blank version column on a provisioned
// device: the metadata probe failed because this CLI has no certificate the
// device accepts (unprovisioned CLI, or logged into a different account).
const discoverNoAccessHint = "This device is provisioned and this CLI does not have access, so agent details cannot be read. Run 'wendy auth login' with an account that can access it."

// lanProvisionedDisplay maps a LAN device's advertised mTLS state to the
// "Provisioned" column value. Non-LAN devices don't advertise this, so nil
// returns "".
func lanProvisionedDisplay(lan *models.LANDevice) string {
	if lan == nil {
		return ""
	}
	if lan.IsMTLS {
		return "Provisioned"
	}
	return "Unprovisioned"
}

// lanNoAccessHint returns discoverNoAccessHint when the device advertises
// mTLS (provisioned) but the agent metadata probe came back empty — the
// signature of a CLI that cannot authenticate to it.
func lanNoAccessHint(lan *models.LANDevice, agentVersion string) string {
	if lan != nil && lan.IsMTLS && agentVersion == "" {
		return discoverNoAccessHint
	}
	return ""
}

// agentBehindCLI reports whether agentVer is an older release than cliVer and
// should be flagged as outdated. A dev build on either side (see version.IsDev)
// is treated as the latest version, so a dev CLI never flags real agents and a
// dev agent is never flagged (WDY-1770). An empty agentVer is treated as
// unknown (not behind).
func agentBehindCLI(cliVer, agentVer string) bool {
	if agentVer == "" || version.IsDev(cliVer) || version.IsDev(agentVer) {
		return false
	}
	return version.CompareVersions(cliVer, agentVer) > 0
}

// cliBehindAgent reports whether cliVer is an older release than agentVer, with
// the same dev-build handling as agentBehindCLI.
func cliBehindAgent(cliVer, agentVer string) bool {
	if agentVer == "" || version.IsDev(cliVer) || version.IsDev(agentVer) {
		return false
	}
	return version.CompareVersions(cliVer, agentVer) < 0
}

// agentAlreadyAtReleaseNote returns a flash note when an explicit update would
// be a no-op: the agent already reports releaseVer or newer. Whether an update
// is needed is a question about the release channel, so it is never answered by
// comparing against this CLI's own version. A dev agent is never current here —
// an explicit update overwrites it with the release, as `wendy device update`
// does. An unknown version on either side means "attempt the update".
func agentAlreadyAtReleaseNote(deviceName, agentVer, releaseVer string) string {
	if releaseVer == "" || version.IsDev(agentVer) || !agentUpdateVerified(agentVer, releaseVer) {
		return ""
	}
	return fmt.Sprintf("%s is already at the latest release (%s).", deviceName, agentVer)
}

// discoverUpdateFlash renders the flash line for a finished update attempt.
func discoverUpdateFlash(msg discoverUpdateDoneMsg) (message string, isError bool) {
	switch {
	case msg.err != nil:
		return fmt.Sprintf("Update failed for %s: %v", msg.deviceName, msg.err), true
	case msg.note != "":
		return msg.note, false
	default:
		return fmt.Sprintf("Updated %s successfully.", msg.deviceName), false
	}
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

func discoverDefaultKey() string {
	if cfg, err := config.Load(); err == nil {
		return strings.ToLower(cfg.DefaultDevice)
	}
	return ""
}

func discoverTableItems(collection *models.DevicesCollection) []discoverTableItem {
	var items []discoverTableItem
	if collection == nil {
		return items
	}
	annotateLANUSBFromEthernet(collection)

	for _, d := range collection.USBDevices {
		deviceType := "USB"
		if d.IsESP32 {
			deviceType = "ESP32"
		}
		items = append(items, discoverTableItem{
			picker: tui.PickerItem{
				Name:          discovery.SanitiseDisplayName(d.DisplayName),
				Type:          deviceType,
				USB:           d.USBVersion,
				Address:       d.Hostname,
				AgentVersion:  discovery.SanitiseDisplayName(d.AgentVersion),
				AgentOutdated: agentBehindCLI(version.Version, d.AgentVersion),
				DedupKey:      d.DisplayName,
				SortKey:       deviceSortKey(d.DisplayName, d.USBVersion),
			},
			info: discoverDeviceInfo{
				Name:    d.DisplayName,
				Type:    deviceType,
				USB:     d.USBVersion,
				Address: d.Hostname,
				Version: d.AgentVersion,
			},
			defaultDevice: firstNonEmpty(d.Hostname, d.DisplayName),
		})
	}
	for _, d := range collection.MergedDevices() {
		deviceType := d.ConnectionTypes()
		usb := ""
		if d.LAN != nil {
			usb = d.LAN.USB
		}
		address := d.Address()
		defaultDevice := d.DisplayName
		lanName := ""
		lanKey := ""
		if d.LAN != nil {
			lanName = d.LAN.DisplayName
			lanKey = discoverycache.Key(d.LAN.ID, d.LAN.DisplayName)
			address = preferredLANAddress(*d.LAN)
			defaultDevice = firstNonEmpty(d.LAN.Hostname, d.LAN.IPAddress, d.LAN.DisplayName)
		}
		provisioned := lanProvisionedDisplay(d.LAN)
		items = append(items, discoverTableItem{
			picker: tui.PickerItem{
				Name:          discovery.SanitiseDisplayName(d.DisplayName),
				Type:          deviceType,
				USB:           usb,
				Address:       address,
				AgentVersion:  discovery.SanitiseDisplayName(d.AgentVersion),
				AgentOutdated: agentBehindCLI(version.Version, d.AgentVersion),
				OS:            d.OS,
				OSVersion:     d.OSVersion,
				Provisioned:   provisioned,
				Hint:          lanNoAccessHint(d.LAN, d.AgentVersion),
				DedupKey:      d.DisplayName,
				SortKey:       deviceSortKey(d.DisplayName, usb),
			},
			info: discoverDeviceInfo{
				Name:        d.DisplayName,
				Type:        deviceType,
				USB:         usb,
				Address:     address,
				Version:     d.AgentVersion,
				Provisioned: provisioned,
			},
			lanName:       lanName,
			lanKey:        lanKey,
			defaultDevice: defaultDevice,
		})
	}
	for _, d := range collection.Simulators {
		// The one-shot table has no Simulator tab, so the type column is what
		// tells a VM apart from a device on the network.
		const deviceType = "Simulator"
		address := preferredLANAddress(d)
		items = append(items, discoverTableItem{
			picker: tui.PickerItem{
				Name:          discovery.SanitiseDisplayName(d.DisplayName),
				Type:          deviceType,
				Address:       address,
				AgentVersion:  discovery.SanitiseDisplayName(d.AgentVersion),
				AgentOutdated: agentBehindCLI(version.Version, d.AgentVersion),
				OS:            d.OS,
				OSVersion:     d.OSVersion,
				DedupKey:      d.ID,
				SortKey:       deviceSortKey(d.DisplayName, ""),
			},
			info: discoverDeviceInfo{
				Name:    d.DisplayName,
				Type:    deviceType,
				Address: address,
				Version: d.AgentVersion,
			},
			defaultDevice: d.ID,
		})
	}
	for _, d := range collection.ExternalDevices {
		// Wendy Lite devices are merged with BLE Lite in MergedDevices().
		if d.ProviderKey == "wendy-lite" {
			continue
		}
		addr := externalProviderAddress(d.ProviderKey, d.ID)
		deviceType := externalProviderDisplayName(d.ProviderKey)
		items = append(items, discoverTableItem{
			picker: tui.PickerItem{
				Name:    discovery.SanitiseDisplayName(d.DisplayName),
				Type:    deviceType,
				Address: addr,
				// Not AgentOutdated: a provider reports its own runtime version
				// (e.g. Docker's), which is not comparable to the CLI's.
				AgentVersion: discovery.SanitiseDisplayName(d.AgentVersion),
				OS:           d.OS,
				OSVersion:    d.OSVersion,
				DedupKey:     d.DisplayName,
				SortKey:      externalProviderSortKey(d.ProviderKey, d.DisplayName),
			},
			info: discoverDeviceInfo{
				Name:    d.DisplayName,
				Type:    deviceType,
				Address: addr,
				Version: d.AgentVersion,
			},
			defaultDevice: firstNonEmpty(d.ID, d.DisplayName),
		})
	}

	sort.SliceStable(items, func(i, j int) bool {
		return discoverSortKey(items[i].picker) < discoverSortKey(items[j].picker)
	})

	return items
}

func discoverPickerItems(items []discoverTableItem) []tui.PickerItem {
	pickerItems := make([]tui.PickerItem, 0, len(items))
	for _, item := range items {
		pickerItem := item.picker
		if item.defaultDevice != "" {
			pickerItem.DefaultKeys = append(pickerItem.DefaultKeys, item.defaultDevice)
		}
		pickerItems = append(pickerItems, pickerItem)
	}
	return pickerItems
}

func discoverSortKey(item tui.PickerItem) string {
	if item.SortKey != "" {
		return item.SortKey
	}
	key := item.DedupKey
	if key == "" {
		key = item.Name
	}
	return strings.ToLower(key)
}

func externalProviderDisplayName(key string) string {
	for _, provider := range providers.AllProviders() {
		if provider.Key() == key {
			return provider.DisplayName()
		}
	}
	return key
}

func externalProviderSortKey(providerKey, name string) string {
	switch providerKey {
	case providers.ProviderKeyAppleContainer:
		return "~0_apple_container_" + strings.ToLower(name)
	case providers.ProviderKeyDocker:
		return "~0_docker_" + strings.ToLower(name)
	case providers.ProviderKeyLocal:
		return "~1_" + strings.ToLower(name)
	}
	return ""
}

// externalProviderAddress returns the provider-qualified ID shown in the
// Address column. Local runtime providers have fixed, meaningless IDs, so
// their address is hidden.
func externalProviderAddress(providerKey, id string) string {
	switch providerKey {
	case providers.ProviderKeyAppleContainer, providers.ProviderKeyDocker, providers.ProviderKeyLocal:
		return ""
	}
	return fmt.Sprintf("%s: %s", providerKey, id)
}

func externalProviderPickerHint(providerKey string) string {
	switch providerKey {
	case providers.ProviderKeyAppleContainer:
		return "Hint: Use Apple Container for local Dockerfile/Containerfile runs on Apple silicon Macs. Compose projects still require Docker."
	case providers.ProviderKeyDocker:
		return "Hint: Use Docker for local container or Compose runs when you do not need WendyOS hardware."
	case providers.ProviderKeyLocal:
		return fmt.Sprintf("Hint: Use %s for native Swift, Go, or Python apps that should run directly on this computer.", providers.LocalDisplayName())
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// copyDeviceJSON marshals v as indented JSON, copies it to the clipboard,
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
		// Non-blocking check: if the command completed at the same moment the
		// timer fired (select race), return the real exit status instead of a
		// spurious timeout error.
		select {
		case err := <-done:
			return err
		default:
		}
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

// errCreateSimulator asks the discover loop to create a VM and come back. Not a
// sentinel: the name travels with it, and the TUI has to be gone before the
// download starts so its progress program can own the terminal.
type errCreateSimulator struct{ name string }

func (e *errCreateSimulator) Error() string { return "create simulator " + e.name }
