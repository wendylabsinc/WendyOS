package commands

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	otelpb "github.com/wendylabsinc/wendy/proto/gen/otelpb"
)

func newDeviceDashboardCmd() *cobra.Command {
	var appName string

	cmd := &cobra.Command{
		Use:   "dashboard",
		Short: "Live dashboard showing metrics and logs from a device",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			m := newDashboardModel(conn, appName, ctx)
			p := tea.NewProgram(m, tea.WithAltScreen())
			if _, err := p.Run(); err != nil {
				return fmt.Errorf("dashboard: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "Filter by application name")
	return cmd
}

// --- Bubble Tea messages ---

type dashboardLogMsg struct {
	service string
	record  *otelpb.LogRecord
}

type dashboardMetricMsg struct {
	service string
	name    string
	unit    string
	value   string
	ts      time.Time
}

type dashboardAppsMsg struct {
	apps []*agentpb.AppContainer
}

type dashboardErrMsg struct{ err error }

// --- Dashboard model ---

type dashboardModel struct {
	conn    *grpcclient.AgentConnection
	ctx     context.Context
	appName string

	logCh    chan dashboardLogMsg
	metricCh chan dashboardMetricMsg
	appsCh   chan dashboardAppsMsg
	errCh    chan error

	logs       []string
	logOffset  int
	autoScroll bool
	metrics    []dashboardMetricEntry
	metricMap  map[string]int
	apps       []*agentpb.AppContainer

	width  int
	height int

	err error
}

type dashboardMetricEntry struct {
	service string
	name    string
	unit    string
	value   string
	ts      time.Time
}

func newDashboardModel(conn *grpcclient.AgentConnection, appName string, ctx context.Context) dashboardModel {
	return dashboardModel{
		conn:       conn,
		ctx:        ctx,
		appName:    appName,
		logCh:      make(chan dashboardLogMsg, 64),
		metricCh:   make(chan dashboardMetricMsg, 64),
		appsCh:     make(chan dashboardAppsMsg, 2),
		errCh:      make(chan error, 4),
		metricMap:  make(map[string]int),
		autoScroll: true,
	}
}

func (m dashboardModel) Init() tea.Cmd {
	// Start background goroutines that push to channels
	go m.runLogStream()
	go m.runMetricStream()
	go m.runAppsPoll()

	return tea.Batch(
		waitForLog(m.logCh),
		waitForMetric(m.metricCh),
		waitForApps(m.appsCh),
		waitForErr(m.errCh),
	)
}

func waitForLog(ch chan dashboardLogMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func waitForMetric(ch chan dashboardMetricMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func waitForApps(ch chan dashboardAppsMsg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return nil
		}
		return msg
	}
}

func waitForErr(ch chan error) tea.Cmd {
	return func() tea.Msg {
		err, ok := <-ch
		if !ok {
			return nil
		}
		return dashboardErrMsg{err}
	}
}

func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			m.autoScroll = false
			if m.logOffset > 0 {
				m.logOffset--
			}
		case "down", "j":
			maxOff := len(m.logs) - m.logViewHeight()
			if maxOff < 0 {
				maxOff = 0
			}
			if m.logOffset < maxOff {
				m.logOffset++
			}
			if m.logOffset >= maxOff {
				m.autoScroll = true
			}
		case "G":
			maxOff := len(m.logs) - m.logViewHeight()
			if maxOff < 0 {
				maxOff = 0
			}
			m.logOffset = maxOff
			m.autoScroll = true
		case "g":
			m.logOffset = 0
			m.autoScroll = false
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

	case dashboardLogMsg:
		line := formatLogLine(msg.service, msg.record)
		m.logs = append(m.logs, line)
		if m.autoScroll {
			maxOff := len(m.logs) - m.logViewHeight()
			if maxOff < 0 {
				maxOff = 0
			}
			m.logOffset = maxOff
		}
		return m, waitForLog(m.logCh)

	case dashboardAppsMsg:
		m.apps = msg.apps
		return m, waitForApps(m.appsCh)

	case dashboardMetricMsg:
		key := msg.service + "/" + msg.name
		entry := dashboardMetricEntry{
			service: msg.service,
			name:    msg.name,
			unit:    msg.unit,
			value:   msg.value,
			ts:      msg.ts,
		}
		if idx, ok := m.metricMap[key]; ok {
			m.metrics[idx] = entry
		} else {
			m.metricMap[key] = len(m.metrics)
			m.metrics = append(m.metrics, entry)
		}
		return m, waitForMetric(m.metricCh)

	case dashboardErrMsg:
		m.err = msg.err
		return m, tea.Quit
	}

	return m, nil
}

func (m dashboardModel) logViewHeight() int {
	// title(2) + blank(1) + footer(1)
	available := m.height - 4
	if available < 1 {
		available = 1
	}
	return available
}

func (m dashboardModel) metricsWidth() int {
	w := m.width
	if w == 0 {
		w = 80
	}
	// Left pane gets 1/3 of width, min 30
	mw := w / 3
	if mw < 30 {
		mw = 30
	}
	if mw > w-20 {
		mw = w - 20
	}
	return mw
}

func (m dashboardModel) logsWidth() int {
	w := m.width
	if w == 0 {
		w = 80
	}
	return w - m.metricsWidth() - 1 // 1 for the separator column
}

var (
	dashTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(tui.ColorPrimary)
	dashHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(tui.Emerald300)
	dashDimStyle    = lipgloss.NewStyle().Foreground(tui.ColorDim)
	dashMetricName  = lipgloss.NewStyle().Foreground(tui.Emerald200)
	dashMetricVal   = lipgloss.NewStyle().Foreground(tui.Emerald400)
	dashMetricUnit  = lipgloss.NewStyle().Foreground(tui.ColorDim)
	dashMetricTime  = lipgloss.NewStyle().Foreground(tui.ColorDim)
	dashFooterStyle = lipgloss.NewStyle().Foreground(tui.ColorDim)
	dashDotGreen    = lipgloss.NewStyle().Foreground(tui.Emerald400).Render("●")
	dashDotBlue     = lipgloss.NewStyle().Foreground(tui.Emerald300).Render("●")
)

func truncateVisible(s string, maxWidth int) string {
	visible := 0
	inEscape := false
	lastSafe := len(s)
	for i, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		if visible >= maxWidth {
			lastSafe = i
			break
		}
		visible++
	}
	if visible < maxWidth {
		return s
	}
	return s[:lastSafe] + "\x1b[0m"
}

func padVisible(s string, width int) string {
	visible := 0
	inEscape := false
	for _, r := range s {
		if r == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		visible++
	}
	if visible >= width {
		return s
	}
	return s + strings.Repeat(" ", width-visible)
}

func (m dashboardModel) View() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v\n", m.err)
	}

	w := m.width
	if w == 0 {
		w = 80
	}
	h := m.height
	if h == 0 {
		h = 24
	}

	mw := m.metricsWidth()
	lw := m.logsWidth()
	viewH := m.logViewHeight()

	// Build left pane lines (apps + metrics)
	var leftLines []string

	// Apps section
	leftLines = append(leftLines, dashHeaderStyle.Render(dashDotGreen+" APPS"))
	leftLines = append(leftLines, dashDimStyle.Render(strings.Repeat("─", mw)))
	if len(m.apps) == 0 {
		leftLines = append(leftLines, dashDimStyle.Render("  No apps"))
	} else {
		for _, app := range m.apps {
			stateStr := app.GetRunningState().String()
			dot := dashDimStyle.Render("○")
			nameStyle := dashDimStyle
			if app.GetRunningState() == agentpb.AppRunningState_RUNNING {
				dot = dashDotGreen
				nameStyle = dashMetricName
			}
			line := fmt.Sprintf(" %s %s", dot, nameStyle.Render(app.GetAppName()))
			if v := app.GetAppVersion(); v != "" {
				line += " " + dashDimStyle.Render(v)
			}
			line += " " + dashDimStyle.Render(stateStr)
			if fc := app.GetFailureCount(); fc > 0 {
				line += " " + lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Render(fmt.Sprintf("(%d failures)", fc))
			}
			leftLines = append(leftLines, line)
		}
	}
	leftLines = append(leftLines, "")

	// Metrics section
	leftLines = append(leftLines, dashHeaderStyle.Render(dashDotBlue+" METRICS"))
	leftLines = append(leftLines, dashDimStyle.Render(strings.Repeat("─", mw)))
	if len(m.metrics) == 0 {
		leftLines = append(leftLines, dashDimStyle.Render("  Waiting for metrics..."))
	} else {
		for _, entry := range m.metrics {
			ts := dashMetricTime.Render(entry.ts.Local().Format("15:04:05"))
			name := dashMetricName.Render(entry.name)
			val := dashMetricVal.Render(entry.value)
			unit := ""
			if entry.unit != "" {
				unit = " " + dashMetricUnit.Render(entry.unit)
			}
			line := fmt.Sprintf(" %s %s", ts, name)
			leftLines = append(leftLines, line)
			leftLines = append(leftLines, fmt.Sprintf("   %s%s", val, unit))
		}
	}

	// Build right pane lines (logs)
	var rightLines []string
	rightLines = append(rightLines, dashHeaderStyle.Render(dashDotBlue+" LOGS"))
	rightLines = append(rightLines, dashDimStyle.Render(strings.Repeat("─", lw)))
	if len(m.logs) == 0 {
		rightLines = append(rightLines, dashDimStyle.Render("  Waiting for logs..."))
	} else {
		start := m.logOffset
		end := start + viewH - 2 // subtract header lines
		if end > len(m.logs) {
			end = len(m.logs)
		}
		if start > end {
			start = end
		}
		for i := start; i < end; i++ {
			rightLines = append(rightLines, m.logs[i])
		}
	}

	sep := dashDimStyle.Render("│")

	var b strings.Builder

	// Title bar
	title := " WENDY DEVICE DASHBOARD "
	pad := w - len(title)
	if pad < 0 {
		pad = 0
	}
	lp := pad / 2
	rp := pad - lp
	b.WriteString(dashTitleStyle.Render(strings.Repeat("═", lp) + title + strings.Repeat("═", rp)))
	b.WriteByte('\n')

	// Body rows: combine left and right panes
	totalRows := viewH
	for row := 0; row < totalRows; row++ {
		var leftStr, rightStr string
		if row < len(leftLines) {
			leftStr = truncateVisible(leftLines[row], mw)
		}
		if row < len(rightLines) {
			rightStr = truncateVisible(rightLines[row], lw)
		}
		b.WriteString(padVisible(leftStr, mw))
		b.WriteString(sep)
		b.WriteString(rightStr)
		b.WriteByte('\n')
	}

	// Footer
	b.WriteString(dashFooterStyle.Render("q/Ctrl+C exit | ↑/↓ scroll logs | G/g end/start"))

	return b.String()
}

// --- Background stream goroutines ---

func (m dashboardModel) runLogStream() {
	req := &agentpb.StreamLogsRequest{}
	if m.appName != "" {
		req.AppName = &m.appName
	}
	stream, err := m.conn.TelemetryService.StreamLogs(m.ctx, req)
	if err != nil {
		m.errCh <- fmt.Errorf("starting log stream: %w", err)
		return
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			m.errCh <- fmt.Errorf("receiving logs: %w", err)
			return
		}
		logs := resp.GetLogs()
		if logs == nil {
			continue
		}
		for _, rl := range logs.GetResourceLogs() {
			svc := resourceServiceName(rl.GetResource())
			for _, sl := range rl.GetScopeLogs() {
				for _, lr := range sl.GetLogRecords() {
					m.logCh <- dashboardLogMsg{service: svc, record: lr}
				}
			}
		}
	}
}

func (m dashboardModel) runMetricStream() {
	req := &agentpb.StreamMetricsRequest{}
	if m.appName != "" {
		req.AppName = &m.appName
	}
	stream, err := m.conn.TelemetryService.StreamMetrics(m.ctx, req)
	if err != nil {
		m.errCh <- fmt.Errorf("starting metrics stream: %w", err)
		return
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			m.errCh <- fmt.Errorf("receiving metrics: %w", err)
			return
		}
		metrics := resp.GetMetrics()
		if metrics == nil {
			continue
		}
		for _, rm := range metrics.GetResourceMetrics() {
			svc := resourceServiceName(rm.GetResource())
			for _, sm := range rm.GetScopeMetrics() {
				for _, metric := range sm.GetMetrics() {
					val, ts := extractMetricValue(metric)
					if val != "" {
						m.metricCh <- dashboardMetricMsg{
							service: svc,
							name:    metric.GetName(),
							unit:    metric.GetUnit(),
							value:   val,
							ts:      ts,
						}
					}
				}
			}
		}
	}
}

func (m dashboardModel) runAppsPoll() {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	fetch := func() {
		stream, err := m.conn.ContainerService.ListContainers(m.ctx, &agentpb.ListContainersRequest{})
		if err != nil {
			m.errCh <- fmt.Errorf("listing apps: %w", err)
			return
		}
		var apps []*agentpb.AppContainer
		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				m.errCh <- fmt.Errorf("receiving apps: %w", err)
				return
			}
			if c := resp.GetContainer(); c != nil {
				apps = append(apps, c)
			}
		}
		m.appsCh <- dashboardAppsMsg{apps: apps}
	}

	fetch() // initial fetch
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			fetch()
		}
	}
}

func extractMetricValue(m *otelpb.Metric) (string, time.Time) {
	var pts []*otelpb.NumberDataPoint
	switch {
	case m.GetGauge() != nil:
		pts = m.GetGauge().GetDataPoints()
	case m.GetSum() != nil:
		pts = m.GetSum().GetDataPoints()
	}
	if len(pts) == 0 {
		if h := m.GetHistogram(); h != nil && len(h.GetDataPoints()) > 0 {
			dp := h.GetDataPoints()[len(h.GetDataPoints())-1]
			ts := time.Unix(0, int64(dp.GetTimeUnixNano()))
			return fmt.Sprintf("count=%d sum=%g", dp.GetCount(), dp.GetSum()), ts
		}
		return "", time.Time{}
	}
	dp := pts[len(pts)-1]
	ts := time.Unix(0, int64(dp.GetTimeUnixNano()))
	switch dp.GetValue().(type) {
	case *otelpb.NumberDataPoint_AsDouble:
		return fmt.Sprintf("%g", dp.GetAsDouble()), ts
	case *otelpb.NumberDataPoint_AsInt:
		return fmt.Sprintf("%d", dp.GetAsInt()), ts
	default:
		return "?", ts
	}
}

func formatLogLine(service string, lr *otelpb.LogRecord) string {
	ts := time.Unix(0, int64(lr.GetTimeUnixNano())).Local().Format("15:04:05")
	label, style := severityLabel(lr.GetSeverityNumber())

	var b strings.Builder
	b.WriteString(logTimeStyle.Render(ts))
	b.WriteByte(' ')
	b.WriteString(style.Render(label))
	if service != "" {
		b.WriteByte(' ')
		b.WriteString(logAppStyle.Render("[" + service + "]"))
	}

	body := lr.GetBody()
	if body != nil {
		b.WriteByte(' ')
		b.WriteString(body.GetStringValue())
	}

	attrs := lr.GetAttributes()
	if len(attrs) > 0 {
		b.WriteByte(' ')
		for i, kv := range attrs {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(logMetaStyle.Render(kv.GetKey() + "=" + anyValueString(kv.GetValue())))
		}
	}

	return b.String()
}
