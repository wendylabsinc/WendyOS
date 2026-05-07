package commands

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dashboardRow holds merged data for one app displayed in the dashboard table.
type dashboardRow struct {
	name         string
	version      string
	state        string
	memoryBytes  int64
	storageBytes int64
	volumeCount  int
	volumeBytes  int64
	failures     uint32
	hasStats     bool
	hasVolumes   bool
}

// buildDashboardRows merges containers, stats, and volume data into display rows.
// Order follows the containers slice.
func buildDashboardRows(
	containers []*agentpb.AppContainer,
	stats []*agentpb.ContainerStats,
	volumes []*agentpb.VolumeInfo,
) []dashboardRow {
	// Index stats by app name.
	statsMap := make(map[string]*agentpb.ContainerStats, len(stats))
	for _, s := range stats {
		statsMap[s.GetAppName()] = s
	}

	// Accumulate volume counts and sizes per app name.
	volCount := make(map[string]int)
	volBytes := make(map[string]int64)
	for _, v := range volumes {
		for _, app := range v.GetUsedBy() {
			volCount[app]++
			volBytes[app] += v.GetSizeBytes()
		}
	}

	rows := make([]dashboardRow, len(containers))
	for i, c := range containers {
		name := c.GetAppName()
		row := dashboardRow{
			name:        name,
			version:     c.GetAppVersion(),
			state:       c.GetRunningState().String(),
			failures:    c.GetFailureCount(),
			volumeCount: volCount[name],
			volumeBytes: volBytes[name],
		}
		if s, ok := statsMap[name]; ok {
			row.hasStats = true
			row.memoryBytes = s.GetMemoryBytes()
			row.storageBytes = s.GetStorageBytes()
		}
		if volCount[name] > 0 {
			row.hasVolumes = true
		}
		rows[i] = row
	}
	return rows
}

// --- Message types ---

type appsDashContainersMsg struct {
	containers []*agentpb.AppContainer
	err        error
}

type appsDashStatsMsg struct {
	stats []*agentpb.ContainerStats
	err   error
}

type appsDashVolumesMsg struct {
	volumes []*agentpb.VolumeInfo
	err     error
}

type appsDashActionResultMsg struct {
	text string
	err  error
}

type appsDashClearFlashMsg struct{}

// --- Post-quit action enum ---

type appsDashAction int

const (
	appsDashActionNone appsDashAction = iota
	appsDashActionLogs
)

// --- Model ---

type appsDashboardModel struct {
	conn *grpcclient.AgentConnection
	ctx  context.Context

	// Data channels fed by background goroutines.
	containersCh chan appsDashContainersMsg
	statsCh      chan appsDashStatsMsg
	volumesCh    chan appsDashVolumesMsg

	// Cached data — each updated independently as polls return.
	cachedContainers []*agentpb.AppContainer
	cachedStats      []*agentpb.ContainerStats
	cachedVolumes    []*agentpb.VolumeInfo

	// Current data.
	rows  []dashboardRow
	table bubbleTable.Model

	// UI state.
	flash  string
	height int

	// Embedded confirm state for r / R.
	confirming    bool
	confirmText   string
	confirmAction func() tea.Cmd

	// Post-quit action.
	selectedApp string
	action      appsDashAction

	// Optional callback: called when the user presses 'd'.
	OnSetDefault func()
}

func newAppsDashboardModel(conn *grpcclient.AgentConnection, ctx context.Context) appsDashboardModel {
	return appsDashboardModel{
		conn:         conn,
		ctx:          ctx,
		containersCh: make(chan appsDashContainersMsg, 2),
		statsCh:      make(chan appsDashStatsMsg, 2),
		volumesCh:    make(chan appsDashVolumesMsg, 2),
		table:        tui.NewBubbleTable(true, nil),
	}
}

func (m appsDashboardModel) Init() tea.Cmd {
	go m.runContainersPoll()
	go m.runStatsPoll()
	go m.runVolumesPoll()
	return tea.Batch(
		waitForAppsDashContainers(m.containersCh),
		waitForAppsDashStats(m.statsCh),
		waitForAppsDashVolumes(m.volumesCh),
	)
}

// --- Channel waiters (tea.Cmd that blocks until the next message arrives) ---

func waitForAppsDashContainers(ch chan appsDashContainersMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func waitForAppsDashStats(ch chan appsDashStatsMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func waitForAppsDashVolumes(ch chan appsDashVolumesMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

// --- Polling goroutines ---

func (m appsDashboardModel) runContainersPoll() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	fetch := func() {
		stream, err := m.conn.ContainerService.ListContainers(m.ctx, &agentpb.ListContainersRequest{})
		if err != nil {
			select {
			case m.containersCh <- appsDashContainersMsg{err: err}:
			case <-m.ctx.Done():
			}
			return
		}
		var containers []*agentpb.AppContainer
		var recvErr error
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				recvErr = err
				break
			}
			if c := resp.GetContainer(); c != nil {
				containers = append(containers, c)
			}
		}
		if recvErr != nil {
			select {
			case m.containersCh <- appsDashContainersMsg{err: recvErr}:
			case <-m.ctx.Done():
			}
			return
		}
		select {
		case m.containersCh <- appsDashContainersMsg{containers: containers}:
		case <-m.ctx.Done():
		}
	}

	fetch()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			fetch()
		}
	}
}

func (m appsDashboardModel) runStatsPoll() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	fetch := func() bool {
		resp, err := m.conn.ContainerService.ListContainerStats(m.ctx, &agentpb.ListContainerStatsRequest{})
		if err != nil {
			if status.Code(err) == codes.Unimplemented {
				// Server doesn't support this RPC yet; show "—" for RAM/Storage silently.
				select {
				case m.statsCh <- appsDashStatsMsg{}:
				case <-m.ctx.Done():
				}
				return false // stop polling
			}
			select {
			case m.statsCh <- appsDashStatsMsg{err: err}:
			case <-m.ctx.Done():
			}
			return true
		}
		select {
		case m.statsCh <- appsDashStatsMsg{stats: resp.GetStats()}:
		case <-m.ctx.Done():
		}
		return true
	}

	if !fetch() {
		return
	}
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			if !fetch() {
				return
			}
		}
	}
}

func (m appsDashboardModel) runVolumesPoll() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	fetch := func() {
		resp, err := m.conn.ContainerService.ListVolumes(m.ctx, &agentpb.ListVolumesRequest{})
		if err != nil {
			select {
			case m.volumesCh <- appsDashVolumesMsg{err: err}:
			case <-m.ctx.Done():
			}
			return
		}
		select {
		case m.volumesCh <- appsDashVolumesMsg{volumes: resp.GetVolumes()}:
		case <-m.ctx.Done():
		}
	}

	fetch()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			fetch()
		}
	}
}

// refreshTable rebuilds the bubble-table columns and rows from cached state.
func (m *appsDashboardModel) refreshTable() {
	m.rows = buildDashboardRows(m.cachedContainers, m.cachedStats, m.cachedVolumes)

	cols := []bubbleTable.Column{
		{Title: "", Width: 2},
		{Title: "Name", Width: 30},
		{Title: "Version", Width: 10},
		{Title: "RAM", Width: 9},
		{Title: "Storage", Width: 9},
		{Title: "Vols", Width: 5},
		{Title: "Vol. Usage", Width: 10},
		{Title: "Failures", Width: 8},
	}

	rows := make([]bubbleTable.Row, len(m.rows))
	for i, r := range m.rows {
		icon := "○"
		if r.state == "RUNNING" {
			icon = "●"
		}
		ram := "—"
		if r.hasStats {
			ram = formatBytes(r.memoryBytes)
		}
		storage := "—"
		if r.hasStats {
			storage = formatBytes(r.storageBytes)
		}
		vols := "—"
		volUsage := "—"
		if r.volumeCount > 0 {
			vols = fmt.Sprintf("%d", r.volumeCount)
			volUsage = formatBytes(r.volumeBytes)
		}
		rows[i] = bubbleTable.Row{
			icon,
			r.name,
			r.version,
			ram,
			storage,
			vols,
			volUsage,
			fmt.Sprintf("%d", r.failures),
		}
	}

	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	if m.height > 0 {
		tableH := max(m.height-5, 1)
		m.table.SetHeight(min(len(rows)+1, tableH))
	}
}

// --- Update ---

func (m appsDashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.refreshTable()
		return m, nil

	case appsDashContainersMsg:
		if msg.err != nil {
			m.flash = fmt.Sprintf("Poll error: %s", msg.err)
		} else {
			m.cachedContainers = msg.containers
			m.refreshTable()
		}
		return m, waitForAppsDashContainers(m.containersCh)

	case appsDashStatsMsg:
		if msg.err != nil {
			m.flash = fmt.Sprintf("Poll error: %s", msg.err)
		} else {
			m.cachedStats = msg.stats
			m.refreshTable()
		}
		return m, waitForAppsDashStats(m.statsCh)

	case appsDashVolumesMsg:
		if msg.err != nil {
			m.flash = fmt.Sprintf("Poll error: %s", msg.err)
		} else {
			m.cachedVolumes = msg.volumes
			m.refreshTable()
		}
		return m, waitForAppsDashVolumes(m.volumesCh)

	case appsDashActionResultMsg:
		if msg.err != nil {
			m.flash = fmt.Sprintf("Error: %s", msg.err)
		} else {
			m.flash = msg.text
		}
		return m, func() tea.Msg {
			time.Sleep(3 * time.Second)
			return appsDashClearFlashMsg{}
		}

	case appsDashClearFlashMsg:
		m.flash = ""
		return m, nil

	case tea.KeyMsg:
		// While confirming, only y/n/esc are active.
		if m.confirming {
			switch msg.String() {
			case "y", "Y":
				cmd := m.confirmAction()
				m.confirming = false
				m.confirmText = ""
				m.confirmAction = nil
				return m, cmd
			case "n", "N", "esc":
				m.confirming = false
				m.confirmText = ""
				m.confirmAction = nil
			}
			return m, nil
		}

		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit

		case "enter":
			cursor := m.table.Cursor()
			if cursor >= 0 && cursor < len(m.rows) {
				m.selectedApp = m.rows[cursor].name
				m.action = appsDashActionLogs
			}
			return m, tea.Quit

		case "s":
			cursor := m.table.Cursor()
			if cursor < 0 || cursor >= len(m.rows) {
				return m, nil
			}
			appName := m.rows[cursor].name
			m.flash = fmt.Sprintf("Starting %s…", appName)
			return m, func() tea.Msg {
				stream, err := m.conn.ContainerService.StartContainer(m.ctx, &agentpb.StartContainerRequest{
						AppName:       appName,
						RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
					})
				if err != nil {
					return appsDashActionResultMsg{err: fmt.Errorf("starting %s: %w", appName, err)}
				}
				for {
					_, err := stream.Recv()
					if err == io.EOF {
						break
					}
					if err != nil {
						return appsDashActionResultMsg{err: fmt.Errorf("starting %s: %w", appName, err)}
					}
				}
				return appsDashActionResultMsg{text: fmt.Sprintf("Started %s", appName)}
			}

		case "x":
			cursor := m.table.Cursor()
			if cursor < 0 || cursor >= len(m.rows) {
				return m, nil
			}
			appName := m.rows[cursor].name
			m.flash = fmt.Sprintf("Stopping %s…", appName)
			return m, func() tea.Msg {
				_, err := m.conn.ContainerService.StopContainer(m.ctx, &agentpb.StopContainerRequest{AppName: appName})
				if err != nil {
					return appsDashActionResultMsg{err: fmt.Errorf("stopping %s: %w", appName, err)}
				}
				return appsDashActionResultMsg{text: fmt.Sprintf("Stopped %s", appName)}
			}

		case "r":
			cursor := m.table.Cursor()
			if cursor < 0 || cursor >= len(m.rows) {
				return m, nil
			}
			appName := m.rows[cursor].name
			m.confirming = true
			m.confirmText = fmt.Sprintf("Remove %s? [y/N]", appName)
			m.confirmAction = func() tea.Cmd {
				return func() tea.Msg {
					_, err := m.conn.ContainerService.DeleteContainer(m.ctx, &agentpb.DeleteContainerRequest{
						AppName: appName,
					})
					if err != nil {
						return appsDashActionResultMsg{err: fmt.Errorf("removing %s: %w", appName, err)}
					}
					return appsDashActionResultMsg{text: fmt.Sprintf("Removed %s", appName)}
				}
			}
			return m, nil

		case "R":
			cursor := m.table.Cursor()
			if cursor < 0 || cursor >= len(m.rows) {
				return m, nil
			}
			appName := m.rows[cursor].name
			m.confirming = true
			m.confirmText = fmt.Sprintf("Remove %s and delete volumes? [y/N]", appName)
			m.confirmAction = func() tea.Cmd {
				return func() tea.Msg {
					_, err := m.conn.ContainerService.DeleteContainer(m.ctx, &agentpb.DeleteContainerRequest{
						AppName:       appName,
						DeleteVolumes: true,
					})
					if err != nil {
						return appsDashActionResultMsg{err: fmt.Errorf("removing %s: %w", appName, err)}
					}
					return appsDashActionResultMsg{text: fmt.Sprintf("Removed %s and volumes", appName)}
				}
			}
			return m, nil

		case "d":
			if m.OnSetDefault != nil {
				m.OnSetDefault()
			}
			return m, nil

		default:
			var cmd tea.Cmd
			m.table, cmd = m.table.Update(msg)
			return m, cmd
		}
	}

	return m, nil
}

// --- View ---

func (m appsDashboardModel) View() string {
	var sb strings.Builder

	// Hint line
	hint := "↑/↓ navigate  s start  x stop  r remove  R remove+vols  enter logs  d default  q quit"
	sb.WriteString(dashDimStyle.Render(hint) + "\n\n")

	// Table or empty state
	if len(m.rows) == 0 {
		sb.WriteString(dashDimStyle.Render("  No applications found. Polling…") + "\n")
	} else {
		sb.WriteString(m.table.View() + "\n")
	}

	// Status line
	running, stopped := 0, 0
	for _, r := range m.rows {
		if r.state == "RUNNING" {
			running++
		} else {
			stopped++
		}
	}
	status := fmt.Sprintf("\n  %d apps  ● %d running  ○ %d stopped  (refreshes every 2s)",
		len(m.rows), running, stopped)
	sb.WriteString(dashDimStyle.Render(status) + "\n")

	// Flash / confirm line
	if m.confirming {
		sb.WriteString(dashDimStyle.Render("  "+m.confirmText) + "\n")
	} else if m.flash != "" {
		sb.WriteString(dashMetricVal.Render("  "+m.flash) + "\n")
	}

	return sb.String()
}
