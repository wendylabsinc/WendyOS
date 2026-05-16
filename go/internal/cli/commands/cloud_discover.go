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
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
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
			if len(auth.Certificates) == 0 {
				return fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
			}

			if jsonOutput || !isInteractiveTerminal() {
				return cloudDiscoverJSON(ctx, auth, all)
			}

			m := newCloudDiscoverModel(ctx, auth, brokerURL, all, false, nil)
			p := tea.NewProgram(m)
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("TUI error: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (required when multiple auth sessions exist)")
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
	table          bubbleTable.Model
	quitting       bool
	flashMessage   string
	flashIsError   bool
	updatingName   string
	selected       *cloudpb.Asset
	windowHeight   int
	err            error
	hasResults     bool
}

// newCloudDiscoverModel creates a cloud discover model.
// initialAssets pre-populates the list; when nil the model fetches on init.
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
		return cloudScanMsg{assets: assets, err: err}
	}
}

func (m cloudDiscoverModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.windowHeight = msg.Height
		m.refreshTable()
		return m, nil

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
			m.flashMessage = "Updating " + asset.GetName() + "..."
			m.flashIsError = false
			return m, m.startCloudUpdateCmd(asset)
		}
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd

	case discoverUpdateDoneMsg:
		m.updatingName = ""
		if msg.err != nil {
			m.flashMessage = fmt.Sprintf("Update failed for %s: %v", msg.deviceName, msg.err)
			m.flashIsError = true
		} else {
			m.flashMessage = fmt.Sprintf("Updated %s successfully.", msg.deviceName)
			m.flashIsError = false
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
		sb.WriteString(scanStyle.Render("⟳ Fetching cloud devices...") + "\n")
		sb.WriteString(dimStyle.Render("  ↑/↓ navigate, enter select, u update, q quit") + "\n")
	} else {
		sb.WriteString(scanStyle.Render("⟳ Scanning for cloud devices...") + "\n")
		if m.updatingName != "" {
			sb.WriteString(dimStyle.Render("  updating "+m.updatingName+"... (q quit)") + "\n")
		} else {
			sb.WriteString(dimStyle.Render("  ↑/↓ navigate, enter copy, a copy all, u update, q quit") + "\n")
		}
	}

	sb.WriteString("\n")

	if m.err != nil {
		sb.WriteString(fmt.Sprintf("Error: %v\n", m.err))
	}
	if len(m.assets) > 0 {
		sb.WriteString(m.table.View() + "\n")
	} else if m.err == nil {
		if m.hasResults {
			if m.all {
				sb.WriteString(dimStyle.Render("No enrolled devices found.") + "\n")
			} else {
				sb.WriteString(dimStyle.Render("No online devices found. Use --all to include offline devices.") + "\n")
			}
		} else {
			sb.WriteString(dimStyle.Render("Fetching devices from cloud...") + "\n")
		}
	}

	if m.flashMessage != "" {
		style := flashStyle
		if m.flashIsError {
			style = flashErrorStyle
		} else if m.updatingName != "" {
			style = scanStyle
		}
		sb.WriteString("\n" + style.Render("  "+m.flashMessage) + "\n")
	}

	return sb.String()
}

func (m *cloudDiscoverModel) refreshTable() {
	rows := cloudDiscoverTableRows(m.assets, m.versions)
	m.table.SetColumns(discoverTableColumns(rows))
	m.table.SetRows(rows)
	if len(rows) > 0 && m.table.Cursor() < 0 {
		m.table.SetCursor(0)
	}
	m.table.SetWidth(discoverTableWidth(m.table.Columns()))
	m.table.SetHeight(discoverTableHeight(len(rows), m.windowHeight, true))
}

func cloudDiscoverTableRows(assets []*cloudpb.Asset, versions map[int32]*agentpb.GetAgentVersionResponse) []bubbleTable.Row {
	rows := make([]bubbleTable.Row, 0, len(assets))
	for _, a := range assets {
		addr := a.GetIpAddress()
		if addr == "" {
			addr = "—"
		}
		devType := humanReadableDeviceType(a.GetDeviceType())
		ver := "—"
		if v := versions[a.GetId()]; v != nil {
			ver = markOutdated(v.GetVersion())
			if devType == "" {
				devType = humanReadableDeviceType(v.GetDeviceType())
			}
		}
		rows = append(rows, bubbleTable.Row{"", a.GetName(), devType, addr, ver})
	}
	return rows
}

func cloudDeviceInfoFromAsset(a *cloudpb.Asset, ver *agentpb.GetAgentVersionResponse) discoverDeviceInfo {
	info := discoverDeviceInfo{
		Name:    a.GetName(),
		Type:    humanReadableDeviceType(a.GetDeviceType()),
		Address: a.GetIpAddress(),
	}
	if ver != nil {
		if info.Type == "" {
			info.Type = humanReadableDeviceType(ver.GetDeviceType())
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
		release, err := fetchAgentRelease(false)
		if err != nil {
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("fetching release: %w", err)}
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
		agentVer := resp.GetVersion()
		if agentVer != "" && version.CompareVersions(release.TagName, agentVer) <= 0 {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("device is already up to date (%s)", agentVer)}
		}

		if arch == "" {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("device did not report CPU architecture")}
		}

		assetPrefix := fmt.Sprintf("wendy-agent-linux-%s-", arch)
		var releaseAsset *githubReleaseAsset
		for _, a := range release.Assets {
			if strings.HasPrefix(a.Name, assetPrefix) && strings.HasSuffix(a.Name, ".tar.gz") {
				releaseAsset = &a
				break
			}
		}
		if releaseAsset == nil {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("no asset for linux/%s in release %s", arch, release.TagName)}
		}

		binaryData, sigData, err := downloadAgentBinary(*releaseAsset)
		if err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("downloading binary: %w", err)}
		}
		if err := verifyAgentBinary(binaryData, sigData); err != nil {
			conn.Close()
			return discoverUpdateDoneMsg{assetID: id, deviceName: name, err: fmt.Errorf("GPG verification failed: %w", err)}
		}

		h := sha256.Sum256(binaryData)
		sha256Hash := hex.EncodeToString(h[:])

		if err := deviceUpdateUpload(ctx, conn.AgentService, binaryData, sha256Hash, sigData, false); err != nil {
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
func fetchCloudAssetsFiltered(ctx context.Context, auth *config.AuthConfig, onlineOnly bool) ([]*cloudpb.Asset, error) {
	cert := auth.Certificates[0]
	cloudConn, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer cloudConn.Close()

	assetClient := cloudpb.NewAssetServiceClient(cloudConn)
	req := &cloudpb.ListAssetsRequest{
		OrganizationId:  int32(cert.OrganizationID),
		IsComputeDevice: boolPtr(true),
	}
	if onlineOnly {
		req.OnlineOnly = boolPtr(true)
	}

	stream, err := assetClient.ListAssets(cloudContext(ctx, auth), req)
	if err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}

	var assets []*cloudpb.Asset
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
	}
	return assets, nil
}
