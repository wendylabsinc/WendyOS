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
	assets []*cloudpb.Asset
	err    error
}

// cloudAssetVersionMsg carries fetched version metadata for a single cloud asset.
type cloudAssetVersionMsg struct {
	assetID int32
	resp    *agentpb.GetAgentVersionResponse // nil on error
}

type cloudDiscoverModel struct {
	ctx            context.Context
	auth           *config.AuthConfig
	brokerURL      string
	all            bool
	pickerMode     bool
	assets         []*cloudpb.Asset
	versions       map[int32]*agentpb.GetAgentVersionResponse
	versionPending map[int32]bool
	versionSem     chan struct{}
	table          tui.BubbleTable
	quitting       bool
	flashMessage   string
	flashIsError   bool
	updatingName   string
	selected       *cloudpb.Asset
	windowWidth    int
	windowHeight   int
	err            error
	hasResults     bool
	defaultDevice  string
}

func newCloudDiscoverModel(ctx context.Context, auth *config.AuthConfig, brokerURL string, all, pickerMode bool, initialAssets []*cloudpb.Asset) cloudDiscoverModel {
	m := cloudDiscoverModel{
		ctx:            ctx,
		auth:           auth,
		brokerURL:      brokerURL,
		all:            all,
		pickerMode:     pickerMode,
		table:          newDiscoverTable(true),
		versions:       make(map[int32]*agentpb.GetAgentVersionResponse),
		versionPending: make(map[int32]bool),
		versionSem:     make(chan struct{}, 5),
	}
	if initialAssets != nil {
		m.assets = initialAssets
		m.hasResults = true
	}
	if cfg, err := config.Load(); err == nil {
		m.defaultDevice = cfg.DefaultDevice
	}
	m.refreshTable()
	return m
}

func (m cloudDiscoverModel) Init() tea.Cmd {
	if m.hasResults {
		cmds := []tea.Cmd{delayThen(cloudDiscoverRefreshInterval, m.scanCmd())}
		for _, a := range m.assets {
			id := a.GetId()
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
		assets, err := fetchCloudAssetsFiltered(ctx, auth, onlineOnly)
		if err == nil {
			// This refreshes every cloudDiscoverRefreshInterval (10s) while the
			// TUI is open, so this reseeds on every tick — wasteful, but
			// best-effort and cheap enough (one config read + conditional
			// write) not to special-case out of the picker's one true asset
			// roster fetch.
			seedPinsFromAssetsBestEffort(auth, assets)
		}
		return cloudScanMsg{assets: assets, err: err}
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
			m.assets = msg.assets
			m.err = nil
			m.refreshTable()
		}
		m.hasResults = true
		var cmds []tea.Cmd
		cmds = append(cmds, delayThen(cloudDiscoverRefreshInterval, m.scanCmd()))
		for _, a := range m.assets {
			id := a.GetId()
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
		case "d":
			cursor := m.table.Cursor()
			if cursor < 0 || cursor >= len(m.assets) {
				return m, nil
			}
			key, err := cloudDeviceDefault(m.auth, m.assets[cursor])
			if err == nil {
				err = saveDefaultDevice(key)
			}
			m.flashIsError = err != nil
			if err != nil {
				m.flashMessage = err.Error()
			} else {
				m.defaultDevice = key
				m.flashMessage = fmt.Sprintf("Default device set to %s.", m.assets[cursor].GetName())
				m.refreshTable()
			}
			return m, clearFlashAfter(3 * time.Second)
		case "x":
			message, err := unsetPickerDefault()
			m.flashIsError = err != nil
			if err != nil {
				m.flashMessage = err.Error()
			} else {
				m.defaultDevice, m.flashMessage = "", message
				m.refreshTable()
			}
			return m, clearFlashAfter(3 * time.Second)
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "enter":
			cursor := m.table.Cursor()
			if len(m.assets) == 0 || cursor < 0 || cursor >= len(m.assets) {
				return m, nil
			}
			if m.pickerMode {
				m.selected = m.assets[cursor]
				return m, tea.Quit
			}
			info := cloudDeviceInfoFromAsset(m.assets[cursor], m.versions[m.assets[cursor].GetId()])
			m.flashMessage, m.flashIsError = copyDeviceJSON(info)
			return m, clearFlashAfter(5 * time.Second)
		case "a":
			if len(m.assets) > 0 {
				infos := make([]discoverDeviceInfo, 0, len(m.assets))
				for _, a := range m.assets {
					infos = append(infos, cloudDeviceInfoFromAsset(a, m.versions[a.GetId()]))
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
			if len(m.assets) == 0 || cursor < 0 || cursor >= len(m.assets) {
				return m, nil
			}
			asset := m.assets[cursor]
			m.updatingName = asset.GetName()
			m.flashMessage = "Checking " + asset.GetName() + "..."
			m.flashIsError = false
			return m, m.startCloudUpdateCmd(asset)
		}
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd

	case discoverUpdateDoneMsg:
		m.updatingName = ""
		m.flashMessage, m.flashIsError = discoverUpdateFlash(msg)
		switch {
		case msg.err != nil:
			// Nothing learned about the device; leave the cached row alone.
		case msg.note != "":
			// No update happened, but the probe behind the note is fresher than
			// the cached row.
			if msg.version != nil {
				m.versions[msg.assetID] = msg.version
				m.versionPending[msg.assetID] = false
			}
		default:
			// Invalidate cached version so the table shows fresh data after update.
			delete(m.versions, msg.assetID)
			delete(m.versionPending, msg.assetID)
		}
		return m, clearFlashAfter(10 * time.Second)

	case flashClearMsg:
		m.flashMessage = ""
		m.flashIsError = false
	}
	return m, nil
}

func (m cloudDiscoverModel) View() string {
	if m.quitting || m.selected != nil {
		return ""
	}

	var sb strings.Builder

	if m.pickerMode {
		sb.WriteString(m.viewLine(scanStyle.Render("⟳ Fetching cloud devices...")) + "\n")
		hint := "  ↑/↓ navigate, enter select, d default, x clear default, u update, q quit"
		if m.table.CanScroll() {
			hint = "  ↑/↓ navigate, ←/→ scroll, enter select, d default, x clear default, u update, q quit"
		}
		sb.WriteString(m.viewLine(dimStyle.Render(hint)) + "\n")
	} else {
		sb.WriteString(m.viewLine(scanStyle.Render("⟳ Scanning for cloud devices...")) + "\n")
		if m.updatingName != "" {
			sb.WriteString(m.viewLine(dimStyle.Render("  working on "+m.updatingName+"... (q quit)")) + "\n")
		} else {
			hint := "  ↑/↓ navigate, enter copy, a copy all, d default, x clear default, u update, q quit"
			if m.table.CanScroll() {
				hint = "  ↑/↓ navigate, ←/→ scroll, enter copy, a copy all, d default, x clear default, u update, q quit"
			}
			sb.WriteString(m.viewLine(dimStyle.Render(hint)) + "\n")
		}
	}

	sb.WriteString("\n")

	if m.err != nil {
		sb.WriteString(m.viewLine(fmt.Sprintf("Error: %v", m.err)) + "\n")
	}
	if len(m.assets) > 0 {
		sb.WriteString(m.table.View() + "\n")
		for _, asset := range m.assets {
			if key, err := cloudDeviceDefault(m.auth, asset); err == nil && key == m.defaultDevice {
				sb.WriteString(m.viewLine(dimStyle.Render("  ✦ default device")) + "\n")
				break
			}
		}
		if legend := tui.DeviceWarningLegend(cloudLegendItems(m.assets, m.versions)); legend != "" {
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
	rows := cloudDiscoverTableRows(m.assets, m.versions)
	for i, asset := range m.assets {
		if key, err := cloudDeviceDefault(m.auth, asset); err == nil && key == m.defaultDevice {
			rows[i][0] = "✦"
		}
	}
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
	rows := make([]bubbleTable.Row, 0, len(assets))
	for _, a := range assets {
		devType := humanReadableDeviceType(a.GetDeviceType())
		if devType == "" {
			devType = humanReadableOSType(a.GetOsType(), a.GetArchitecture())
		}
		ver := "—"
		if v := versions[a.GetId()]; v != nil {
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
	items := make([]tui.PickerItem, 0, len(assets))
	for _, a := range assets {
		v := versions[a.GetId()]
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

func (m cloudDiscoverModel) fetchVersionCmd(asset *cloudpb.Asset) tea.Cmd {
	ctx := m.ctx
	auth := m.auth
	brokerURL := m.brokerURL
	id := asset.GetId()
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
		conn, err := connectCloudAsset(fetchCtx, auth, asset, brokerURL)
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

func (m cloudDiscoverModel) startCloudUpdateCmd(asset *cloudpb.Asset) tea.Cmd {
	ctx := m.ctx
	auth := m.auth
	brokerURL := m.brokerURL
	name := asset.GetName()
	arch := asset.GetArchitecture()
	id := asset.GetId()

	return func() tea.Msg {
		latestVer, _, err := resolveAgentVersion(false)
		if err != nil {
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("resolving agent version: %w", err)}
		}

		conn, err := connectCloudAsset(ctx, auth, asset, brokerURL)
		if err != nil {
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("connecting to device: %w", err)}
		}

		resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("querying device: %w", err)}
		}
		// Prefer the architecture reported by the running agent; fall back to cloud metadata.
		if cpuArch := resp.GetCpuArchitecture(); cpuArch != "" {
			arch = cpuArch
		}
		if note := agentAlreadyAtReleaseNote(name, resp.GetVersion(), latestVer); note != "" {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, note: note, version: resp}
		}

		if arch == "" {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("device did not report CPU architecture")}
		}
		osName := resp.GetOs()

		binaryData, actualVer, _, err := resolveAgentArtifact(osName, arch, false)
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("resolving agent binary: %w", err)}
		}
		if err := checkDarwinArtifactVersion(osName, latestVer, actualVer); err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: err}
		}

		h := sha256.Sum256(binaryData)
		sha256Hash := hex.EncodeToString(h[:])

		if err := deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash); err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("uploading: %w", err)}
		}
		conn.Close() // agent is restarting

		newConn, err := waitForCloudAgentRestart(ctx, auth, asset, brokerURL)
		if err != nil {
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("waiting for restart: %w", err)}
		}
		newConn.Close()
		return discoverUpdateDoneMsg{assetID: id, deviceName: name}
	}
}

func cloudDiscoverJSON(ctx context.Context, auth *config.AuthConfig, all bool) error {
	assets, err := fetchCloudAssetsFiltered(ctx, auth, !all)
	if err != nil {
		return err
	}
	seedPinsFromAssetsBestEffort(auth, assets)
	infos := make([]discoverDeviceInfo, 0, len(assets))
	for _, a := range assets {
		infos = append(infos, cloudDeviceInfoFromAsset(a, nil))
	}
	data, err := json.MarshalIndent(infos, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(data))
	return nil
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
