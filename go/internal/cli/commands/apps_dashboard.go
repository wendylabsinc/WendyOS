package commands

import (
	"context"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"io"
	"strings"
	"time"

	bubbleTable "github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type dashboardRow struct {
	name          string // app ID for actions; "" for sub-rows
	displayName   string // shown in the Name column
	version       string
	state         string
	memoryBytes   int64
	storageBytes  int64
	volumeCount   int
	volumeBytes   int64
	failures      uint32
	termReason    string // exit reason for a stopped app; "" otherwise
	exitCode      int32
	hasStats      bool
	hasVolumes    bool
	isGroupHeader bool
	isSubrow      bool
}

// buildDashboardRows merges containers, stats, and volume data into display rows.
// Running apps appear first, preserving the device's order within each state
// group. Multi-service apps produce a group-header row followed by one sub-row
// per service; sub-rows are display-only (no actions).
func buildDashboardRows(
	containers []*agentpb.AppContainer,
	stats []*agentpb.ContainerStats,
	volumes []*agentpb.VolumeInfo,
) []dashboardRow {
	orderedContainers := append([]*agentpb.AppContainer(nil), containers...)
	sortRunningFirst(orderedContainers, func(c *agentpb.AppContainer) string {
		return c.GetRunningState().String()
	})

	// Index stats by container ID (set to ctr.ID() by GetContainerStats).
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

	var rows []dashboardRow
	for _, c := range orderedContainers {
		appName := c.GetAppName()
		services := c.GetServices()

		if len(services) > 1 {
			// Sum stats from all service containers (keyed as appID_serviceName).
			var totalMem, totalStorage int64
			hasStats := false
			for _, svc := range services {
				if s, ok := statsMap[appName+"_"+svc.GetName()]; ok {
					totalMem += s.GetMemoryBytes()
					totalStorage += s.GetStorageBytes()
					hasStats = true
				}
			}
			rows = append(rows, dashboardRow{
				name:          appName,
				displayName:   appName + " [group]",
				version:       c.GetAppVersion(),
				state:         c.GetRunningState().String(),
				failures:      c.GetFailureCount(),
				termReason:    c.GetTerminationReason(),
				exitCode:      c.GetExitCode(),
				volumeCount:   volCount[appName],
				volumeBytes:   volBytes[appName],
				hasStats:      hasStats,
				memoryBytes:   totalMem,
				storageBytes:  totalStorage,
				hasVolumes:    volCount[appName] > 0,
				isGroupHeader: true,
			})
			for _, svc := range services {
				var svcMem, svcStorage int64
				hasSvcStats := false
				if s, ok := statsMap[appName+"_"+svc.GetName()]; ok {
					svcMem = s.GetMemoryBytes()
					svcStorage = s.GetStorageBytes()
					hasSvcStats = true
				}
				rows = append(rows, dashboardRow{
					displayName:  "  ↳ " + svc.GetName(),
					state:        svc.GetRunningState().String(),
					memoryBytes:  svcMem,
					storageBytes: svcStorage,
					hasStats:     hasSvcStats,
					isSubrow:     true,
				})
			}
		} else {
			row := dashboardRow{
				name:        appName,
				displayName: appName,
				version:     c.GetAppVersion(),
				state:       c.GetRunningState().String(),
				failures:    c.GetFailureCount(),
				termReason:  c.GetTerminationReason(),
				exitCode:    c.GetExitCode(),
				volumeCount: volCount[appName],
				volumeBytes: volBytes[appName],
			}
			if s, ok := statsMap[appName]; ok {
				row.hasStats = true
				row.memoryBytes = s.GetMemoryBytes()
				row.storageBytes = s.GetStorageBytes()
			}
			if volCount[appName] > 0 {
				row.hasVolumes = true
			}
			rows = append(rows, row)
		}
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
	table tui.BubbleTable

	// UI state.
	width  int
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

func appsDashPollFlash(err error) string {
	if status.Code(err) == codes.Unimplemented {
		return "Poll notice: " + userFacingGRPCError(err)
	}
	return "Poll error: " + userFacingGRPCError(err)
}

func userFacingGRPCError(err error) string {
	if err == nil {
		return ""
	}
	// A broker-closed tunnel: the verdict inside the handshake failure is the
	// whole story, so show it instead of the transport envelope.
	if verdict, ok := clouddefaults.ExplainTunnelClose(err.Error()); ok {
		return "Wendy Cloud closed the tunnel: " + verdict
	}
	if _, ok := status.FromError(err); ok {
		if desc, descOK := grpcDescFromErrorString(err.Error()); descOK {
			return desc
		}
	}
	return err.Error()
}

func grpcDescFromErrorString(msg string) (string, bool) {
	idx := strings.Index(msg, "desc = ")
	if idx < 0 {
		return "", false
	}
	desc := strings.TrimSpace(msg[idx+len("desc = "):])
	return desc, desc != ""
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

	fetch := func() bool {
		resp, err := m.conn.ContainerService.ListVolumes(m.ctx, &agentpb.ListVolumesRequest{})
		if err != nil {
			select {
			case m.volumesCh <- appsDashVolumesMsg{err: err}:
			case <-m.ctx.Done():
			}
			return status.Code(err) != codes.Unimplemented
		}
		select {
		case m.volumesCh <- appsDashVolumesMsg{volumes: resp.GetVolumes()}:
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
		{Title: "Reason", Width: 18},
	}

	rows := make([]bubbleTable.Row, len(m.rows))
	for i, r := range m.rows {
		icon := "○"
		switch r.state {
		case "RUNNING":
			icon = "●"
		case "CRASH_LOOPING":
			icon = "↻"
		}
		ram := "—"
		if r.hasStats {
			ram = formatBytes(r.memoryBytes)
		}
		storage := "—"
		if r.hasStats {
			storage = formatBytes(r.storageBytes)
		}
		if r.isSubrow {
			rows[i] = bubbleTable.Row{icon, r.displayName, "", ram, storage, "", "", "", ""}
			continue
		}
		vols := "—"
		volUsage := "—"
		if r.volumeCount > 0 {
			vols = fmt.Sprintf("%d", r.volumeCount)
			volUsage = formatBytes(r.volumeBytes)
		}
		rows[i] = bubbleTable.Row{
			icon,
			r.displayName,
			r.version,
			ram,
			storage,
			vols,
			volUsage,
			fmt.Sprintf("%d", r.failures),
			terminationSummary(r.termReason, r.exitCode),
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
		m.width = msg.Width
		m.height = msg.Height
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		m.refreshTable()
		return m, cmd

	case appsDashContainersMsg:
		if msg.err != nil {
			m.flash = appsDashPollFlash(msg.err)
		} else {
			m.cachedContainers = msg.containers
			m.refreshTable()
		}
		return m, waitForAppsDashContainers(m.containersCh)

	case appsDashStatsMsg:
		if msg.err != nil {
			m.flash = appsDashPollFlash(msg.err)
		} else {
			m.cachedStats = msg.stats
			m.refreshTable()
		}
		return m, waitForAppsDashStats(m.statsCh)

	case appsDashVolumesMsg:
		if msg.err != nil {
			m.flash = appsDashPollFlash(msg.err)
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
			if cursor >= 0 && cursor < len(m.rows) && !m.rows[cursor].isSubrow {
				m.selectedApp = m.rows[cursor].name
				m.action = appsDashActionLogs
			}
			return m, tea.Quit

		case "s":
			cursor := m.table.Cursor()
			if cursor < 0 || cursor >= len(m.rows) || m.rows[cursor].isSubrow {
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
			if cursor < 0 || cursor >= len(m.rows) || m.rows[cursor].isSubrow {
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
			if cursor < 0 || cursor >= len(m.rows) || m.rows[cursor].isSubrow {
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
			if cursor < 0 || cursor >= len(m.rows) || m.rows[cursor].isSubrow {
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
	if m.table.CanScroll() {
		hint = "↑/↓ navigate  ←/→ scroll  s start  x stop  r remove  R remove+vols  enter logs  d default  q quit"
	}
	sb.WriteString(m.viewLine(dashDimStyle.Render(hint)) + "\n\n")

	// Table or empty state
	if len(m.rows) == 0 {
		sb.WriteString(m.viewLine(dashDimStyle.Render("  No applications found. Polling…")) + "\n")
	} else {
		sb.WriteString(m.table.View() + "\n")
	}

	// Status line (sub-rows don't count as separate apps).
	running, stopped, crashLooping, total := 0, 0, 0, 0
	for _, r := range m.rows {
		if r.isSubrow {
			continue
		}
		total++
		switch r.state {
		case "RUNNING":
			running++
		case "CRASH_LOOPING":
			crashLooping++
		default:
			stopped++
		}
	}
	status := fmt.Sprintf("\n  %d apps  ● %d running  ○ %d stopped", total, running, stopped)
	if crashLooping > 0 {
		status += fmt.Sprintf("  ↻ %d crash-looping", crashLooping)
	}
	status += "  (refreshes every 2s)"
	sb.WriteString(m.viewLine(dashDimStyle.Render(status)) + "\n")

	// Flash / confirm line
	if m.confirming {
		sb.WriteString(m.viewLine(dashDimStyle.Render("  "+m.confirmText)) + "\n")
	} else if m.flash != "" {
		sb.WriteString(m.viewLine(dashMetricVal.Render("  "+m.flash)) + "\n")
	}

	return sb.String()
}

func (m appsDashboardModel) viewLine(line string) string {
	if m.width <= 0 {
		return line
	}
	return tui.CropANSIView(line, 0, m.width)
}
