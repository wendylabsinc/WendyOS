package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const cloudDiscoverRefreshInterval = 10 * time.Second

func newCloudDiscoverCmd() *cobra.Command {
	var cloudGRPC string
	var brokerURL string
	var all bool

	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List enrolled devices in Wendy Cloud",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}
			if jsonOutput || !isInteractiveTerminal() {
				return cloudDiscoverJSON(ctx, auth, all)
			}

			m := newCloudDiscoverModel(ctx, auth, brokerURL, all, false, nil)
			// Alt screen restores the user's terminal content on exit. The
			// picker-mode embedding in `wendy cloud tunnel` stays inline.
			p := tea.NewProgram(m, tea.WithAltScreen())
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (optional when a default session is set via 'wendy auth use')")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: cloud :443 endpoint, otherwise <cloud-host>:50052)")
	cmd.Flags().BoolVar(&all, "all", false, "Include offline devices")
	return cmd
}

// cloudScanMsg carries a refreshed list of cloud assets.
type cloudScanMsg struct {
	assets  []*cloudpb.Asset
	devices []cloudDiscoveryDevice
	err     error
}

// cloudAssetVersionMsg carries fetched version metadata for a single cloud asset.
type cloudAssetVersionMsg struct {
	assetID string
	resp    *agentpb.GetAgentVersionResponse // nil on error
}

type cloudDiscoverModel struct {
	ctx            context.Context
	auth           *config.AuthConfig
	brokerURL      string
	all            bool
	pickerMode     bool
	devices        []cloudDiscoveryDevice
	versions       map[string]*agentpb.GetAgentVersionResponse
	versionPending map[string]bool
	versionSem     chan struct{}
	table          tui.BubbleTable
	quitting       bool
	flashMessage   string
	flashIsError   bool
	updatingName   string
	selected       *cloudpb.Asset
	selectedV2     *cloudpbv2.Asset
	windowWidth    int
	windowHeight   int
	err            error
	hasResults     bool
}

func newCloudDiscoverModel(ctx context.Context, auth *config.AuthConfig, brokerURL string, all, pickerMode bool, initialAssets []*cloudpb.Asset) cloudDiscoverModel {
	m := cloudDiscoverModel{
		ctx:            ctx,
		auth:           auth,
		brokerURL:      brokerURL,
		all:            all,
		pickerMode:     pickerMode,
		table:          newDiscoverTable(true),
		versions:       make(map[string]*agentpb.GetAgentVersionResponse),
		versionPending: make(map[string]bool),
		versionSem:     make(chan struct{}, 5),
	}
	if initialAssets != nil {
		m.devices = legacyDiscoveryDevices(initialAssets)
		m.hasResults = true
	}
	m.refreshTable()
	return m
}

func (m cloudDiscoverModel) Init() tea.Cmd {
	if m.hasResults {
		cmds := []tea.Cmd{delayThen(cloudDiscoverRefreshInterval, m.scanCmd())}
		for _, a := range m.devices {
			id := a.key
			if !m.versionPending[id] {
				if _, cached := m.versions[id]; !cached {
					m.versionPending[id] = true
					cmds = append(cmds, m.fetchVersionCmd(a))
				}
			}
		}
		return tea.Batch(cmds...)
	}
	return m.scanCmd()
}

func (m cloudDiscoverModel) scanCmd() tea.Cmd {
	ctx := m.ctx
	auth := m.auth
	onlineOnly := !m.all
	return func() tea.Msg {
		devices, err := fetchCloudDiscoveryDevices(ctx, auth, onlineOnly)
		return cloudScanMsg{devices: devices, err: err}
	}
}

func (m cloudDiscoverModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.windowWidth = msg.Width
		m.windowHeight = msg.Height
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		m.refreshTable()
		return m, cmd

	case cloudScanMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.devices = msg.devices
			if msg.assets != nil {
				m.devices = legacyDiscoveryDevices(msg.assets)
			}
			m.err = nil
			m.refreshTable()
		}
		m.hasResults = true
		var cmds []tea.Cmd
		cmds = append(cmds, delayThen(cloudDiscoverRefreshInterval, m.scanCmd()))
		for _, a := range m.devices {
			id := a.key
			if !m.versionPending[id] {
				if _, cached := m.versions[id]; !cached {
					m.versionPending[id] = true
					cmds = append(cmds, m.fetchVersionCmd(a))
				}
			}
		}
		return m, tea.Batch(cmds...)

	case cloudAssetVersionMsg:
		m.versionPending[msg.assetID] = false
		if msg.resp != nil {
			m.versions[msg.assetID] = msg.resp
			m.refreshTable()
		}
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			cursor := m.table.Cursor()
			if len(m.devices) == 0 || cursor < 0 || cursor >= len(m.devices) {
				return m, nil
			}
			if m.pickerMode {
				m.selected = m.devices[cursor].legacy
				m.selectedV2 = m.devices[cursor].v2
				return m, tea.Quit
			}
			info := m.devices[cursor].info(m.versions[m.devices[cursor].key])
			m.flashMessage, m.flashIsError = copyDeviceJSON(info)
			return m, clearFlashAfter(5 * time.Second)
		case "a":
			if len(m.devices) > 0 {
				infos := make([]any, 0, len(m.devices))
				for _, a := range m.devices {
					infos = append(infos, a.info(m.versions[a.key]))
				}
				m.flashMessage, m.flashIsError = copyDeviceJSON(infos)
				if !m.flashIsError {
					m.flashMessage = "Copied all devices as JSON to clipboard."
				}
				return m, clearFlashAfter(5 * time.Second)
			}
			return m, nil
		case "u":
			if m.updatingName != "" {
				return m, nil
			}
			cursor := m.table.Cursor()
			if len(m.devices) == 0 || cursor < 0 || cursor >= len(m.devices) {
				return m, nil
			}
			asset := m.devices[cursor]
			m.updatingName = asset.GetName()
			m.flashMessage = "Checking " + asset.GetName() + "..."
			m.flashIsError = false
			return m, m.startCloudUpdateCmd(asset)
		}
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd

	case discoverUpdateDoneMsg:
		key := msg.cloudAssetKey
		if key == "" {
			key = fmt.Sprint(msg.assetID)
		}
		m.updatingName = ""
		m.flashMessage, m.flashIsError = discoverUpdateFlash(msg)
		switch {
		case msg.err != nil:
			// Nothing learned about the device; leave the cached row alone.
		case msg.note != "":
			// No update happened, but the probe behind the note is fresher than
			// the cached row.
			if msg.version != nil {
				m.versions[key] = msg.version
				m.versionPending[key] = false
			}
		default:
			// Invalidate cached version so the table shows fresh data after update.
			delete(m.versions, key)
			delete(m.versionPending, key)
		}
		return m, clearFlashAfter(10 * time.Second)

	case flashClearMsg:
		m.flashMessage = ""
		m.flashIsError = false
	}
	return m, nil
}

func (m cloudDiscoverModel) View() string {
	if m.quitting || m.selected != nil || m.selectedV2 != nil {
		return ""
	}

	var sb strings.Builder

	if m.pickerMode {
		sb.WriteString(m.viewLine(scanStyle.Render("⟳ Fetching cloud devices...")) + "\n")
		hint := "  ↑/↓ navigate, enter select, u update, q quit"
		if m.table.CanScroll() {
			hint = "  ↑/↓ navigate, ←/→ scroll, enter select, u update, q quit"
		}
		sb.WriteString(m.viewLine(dimStyle.Render(hint)) + "\n")
	} else {
		sb.WriteString(m.viewLine(scanStyle.Render("⟳ Scanning for cloud devices...")) + "\n")
		if m.updatingName != "" {
			sb.WriteString(m.viewLine(dimStyle.Render("  working on "+m.updatingName+"... (q quit)")) + "\n")
		} else {
			hint := "  ↑/↓ navigate, enter copy, a copy all, u update, q quit"
			if m.table.CanScroll() {
				hint = "  ↑/↓ navigate, ←/→ scroll, enter copy, a copy all, u update, q quit"
			}
			sb.WriteString(m.viewLine(dimStyle.Render(hint)) + "\n")
		}
	}

	sb.WriteString("\n")

	if m.err != nil {
		sb.WriteString(m.viewLine(fmt.Sprintf("Error: %v", m.err)) + "\n")
	}
	if len(m.devices) > 0 {
		sb.WriteString(m.table.View() + "\n")
		// Cloud rows carry no provisioned/default markers, so the legend exists
		// only to explain a warning glyph that is actually present.
		if legend := tui.DeviceWarningLegend(cloudDiscoveryLegendItems(m.devices, m.versions)); legend != "" {
			sb.WriteString(m.viewLine(dimStyle.Render("  "+legend)) + "\n")
		}
	} else if m.err == nil {
		if m.hasResults {
			if m.all {
				sb.WriteString(m.viewLine(dimStyle.Render("No enrolled devices found.")) + "\n")
			} else {
				sb.WriteString(m.viewLine(dimStyle.Render("No online devices found. Use --all to include offline devices.")) + "\n")
			}
		} else {
			sb.WriteString(m.viewLine(dimStyle.Render("Fetching active devices from cloud...")) + "\n")
		}
	}

	if m.flashMessage != "" {
		style := flashStyle
		if m.flashIsError {
			style = flashErrorStyle
		} else if m.updatingName != "" {
			style = scanStyle
		}
		sb.WriteString("\n" + m.viewLine(style.Render("  "+m.flashMessage)) + "\n")
	}

	return sb.String()
}

func (m *cloudDiscoverModel) refreshTable() {
	rows := cloudDiscoveryTableRows(m.devices, m.versions)
	m.table.SetColumns(discoverTableColumns(rows))
	m.table.SetRows(rows)
	if len(rows) > 0 && m.table.Cursor() < 0 {
		m.table.SetCursor(0)
	}
	m.table.SetWidth(discoverTableWidth(m.table.Columns()))
	m.table.SetHeight(discoverTableHeight(len(rows), m.windowHeight, true))
}

func (m cloudDiscoverModel) viewLine(line string) string {
	if m.windowWidth <= 0 {
		return line
	}
	return tui.CropANSIView(line, 0, m.windowWidth)
}

func cloudDiscoverTableRows(assets []*cloudpb.Asset, versions map[int32]*agentpb.GetAgentVersionResponse) []bubbleTable.Row {
	byID := make(map[string]*agentpb.GetAgentVersionResponse, len(versions))
	for id, ver := range versions {
		byID[fmt.Sprint(id)] = ver
	}
	return cloudDiscoveryTableRows(legacyDiscoveryDevices(assets), byID)
}
func cloudDiscoveryTableRows(assets []cloudDiscoveryDevice, versions map[string]*agentpb.GetAgentVersionResponse) []bubbleTable.Row {
	rows := make([]bubbleTable.Row, 0, len(assets))
	for _, a := range assets {
		devType := humanReadableDeviceType(a.GetDeviceType())
		if devType == "" {
			devType = humanReadableOSType(a.GetOsType(), a.GetArchitecture())
		}
		ver := "—"
		if v := versions[a.key]; v != nil {
			ver = v.GetVersion()
			if agentBehindCLI(version.Version, ver) {
				ver += " " + tui.GlyphOutdated
			}
			if devType == "" {
				devType = humanReadableDeviceType(v.GetDeviceType())
			}
			if devType == "" {
				devType = humanReadableOSType(v.GetOs(), v.GetCpuArchitecture())
			}
		}
		rows = append(rows, bubbleTable.Row{"", a.GetName(), devType, ver})
	}
	return rows
}

// cloudLegendItems reduces cloud rows to the fields the shared legend reads.
func cloudLegendItems(assets []*cloudpb.Asset, versions map[int32]*agentpb.GetAgentVersionResponse) []tui.PickerItem {
	byID := make(map[string]*agentpb.GetAgentVersionResponse, len(versions))
	for id, ver := range versions {
		byID[fmt.Sprint(id)] = ver
	}
	return cloudDiscoveryLegendItems(legacyDiscoveryDevices(assets), byID)
}
func cloudDiscoveryLegendItems(assets []cloudDiscoveryDevice, versions map[string]*agentpb.GetAgentVersionResponse) []tui.PickerItem {
	items := make([]tui.PickerItem, 0, len(assets))
	for _, a := range assets {
		v := versions[a.key]
		if v == nil {
			continue
		}
		items = append(items, tui.PickerItem{
			AgentVersion:  v.GetVersion(),
			AgentOutdated: agentBehindCLI(version.Version, v.GetVersion()),
		})
	}
	return items
}

func cloudDeviceInfoFromAsset(a *cloudpb.Asset, ver *agentpb.GetAgentVersionResponse) discoverDeviceInfo {
	info := discoverDeviceInfo{
		ID:      a.GetId(),
		Name:    a.GetName(),
		Type:    humanReadableDeviceType(a.GetDeviceType()),
		Address: a.GetIpAddress(),
	}
	if info.Type == "" {
		info.Type = humanReadableOSType(a.GetOsType(), a.GetArchitecture())
	}
	if ver != nil {
		if info.Type == "" {
			info.Type = humanReadableDeviceType(ver.GetDeviceType())
		}
		if info.Type == "" {
			info.Type = humanReadableOSType(ver.GetOs(), ver.GetCpuArchitecture())
		}
		info.Version = ver.GetVersion()
	}
	return info
}

const cloudVersionFetchTimeout = 15 * time.Second

func (m cloudDiscoverModel) fetchVersionCmd(asset cloudDiscoveryDevice) tea.Cmd {
	ctx := m.ctx
	auth := m.auth
	brokerURL := m.brokerURL
	id := asset.key
	sem := m.versionSem
	return func() tea.Msg {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return cloudAssetVersionMsg{assetID: id}
		}
		defer func() { <-sem }()

		fetchCtx, cancel := context.WithTimeout(ctx, cloudVersionFetchTimeout)
		defer cancel()
		conn, err := asset.connect(fetchCtx, auth, brokerURL)
		if err != nil {
			return cloudAssetVersionMsg{assetID: id}
		}
		defer conn.Close()
		resp, err := conn.AgentService.GetAgentVersion(fetchCtx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			return cloudAssetVersionMsg{assetID: id}
		}
		return cloudAssetVersionMsg{assetID: id, resp: resp}
	}
}

func (m cloudDiscoverModel) startCloudUpdateCmd(asset cloudDiscoveryDevice) tea.Cmd {
	ctx := m.ctx
	auth := m.auth
	brokerURL := m.brokerURL
	name := asset.GetName()
	arch := asset.GetArchitecture()
	id := asset.key

	return func() tea.Msg {
		latestVer, _, err := resolveAgentVersion(false)
		if err != nil {
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, err: fmt.Errorf("resolving agent version: %w", err)}
		}

		conn, err := asset.connect(ctx, auth, brokerURL)
		if err != nil {
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, err: fmt.Errorf("connecting to device: %w", err)}
		}

		resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, err: fmt.Errorf("querying device: %w", err)}
		}
		// Prefer the architecture reported by the running agent; fall back to cloud metadata.
		if cpuArch := resp.GetCpuArchitecture(); cpuArch != "" {
			arch = cpuArch
		}
		if note := agentAlreadyAtReleaseNote(name, resp.GetVersion(), latestVer); note != "" {
			conn.Close()
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, note: note, version: resp}
		}

		if arch == "" {
			conn.Close()
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, err: fmt.Errorf("device did not report CPU architecture")}
		}
		osName := resp.GetOs()

		binaryData, actualVer, _, err := resolveAgentArtifact(osName, arch, false)
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, err: fmt.Errorf("resolving agent binary: %w", err)}
		}
		if err := checkDarwinArtifactVersion(osName, latestVer, actualVer); err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, err: err}
		}

		h := sha256.Sum256(binaryData)
		sha256Hash := hex.EncodeToString(h[:])

		if err := deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash); err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, err: fmt.Errorf("uploading: %w", err)}
		}
		conn.Close() // agent is restarting

		newConn, err := asset.reconnect(ctx, auth, brokerURL)
		if err != nil {
			return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name, err: fmt.Errorf("waiting for restart: %w", err)}
		}
		newConn.Close()
		return discoverUpdateDoneMsg{cloudAssetKey: id, deviceName: name}
	}
}

func cloudDiscoverJSON(ctx context.Context, auth *config.AuthConfig, all bool) error {
	devices, err := fetchCloudDiscoveryDevices(ctx, auth, !all)
	if err != nil {
		return err
	}
	infos := make([]any, 0, len(devices))
	for _, d := range devices {
		infos = append(infos, d.info(nil))
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(infos)
}

// fetchCloudAssetsFiltered retrieves compute-device assets for the org.
// When onlineOnly is true, only online (enrolled and reachable) assets are returned.
//
// ListAssets is paginated server-side: a single request returns only the first
// page (capped well below a large fleet), so we page through with offset/limit
// until we've collected every asset the server reports via total. Without this,
// callers silently see only the first page — e.g. fleet group operations could
// not target devices that fell outside it.
func fetchCloudAssetsFiltered(ctx context.Context, auth *config.AuthConfig, onlineOnly bool) ([]*cloudpb.Asset, error) {
	cert := auth.Certificates[0]
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer cloudConn.Close()

	assetClient := cloudpb.NewAssetServiceClient(cloudConn)

	const pageSize = 200
	var assets []*cloudpb.Asset
	for offset := int32(0); ; {
		req := &cloudpb.ListAssetsRequest{
			OrganizationId:  int32(cert.OrganizationID),
			IsComputeDevice: boolPtr(true),
			Offset:          int32Ptr(offset),
			Limit:           int32Ptr(pageSize),
		}
		if onlineOnly {
			req.OnlineOnly = boolPtr(true)
		}

		cloudCtx, err := cloudContext(ctx, auth)
		if err != nil {
			return nil, err
		}
		stream, err := assetClient.ListAssets(cloudCtx, req)
		if err != nil {
			return nil, fmt.Errorf("listing devices: %w", err)
		}

		page := 0
		var total int32
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("listing devices: %w", err)
			}
			if len(assets) >= maxCloudAssets {
				return nil, fmt.Errorf("cloud returned more than %d devices", maxCloudAssets)
			}
			assets = append(assets, resp.GetAsset())
			page++
			total = resp.GetTotal()
		}

		offset += int32(page)
		// Done when the server reports we've seen every asset, or a page makes
		// no progress (guards against a missing/inconsistent total).
		if page == 0 || offset >= total {
			break
		}
	}
	return assets, nil
}
