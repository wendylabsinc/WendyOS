package commands

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
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
	record  *logspb.LogRecord
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
	logHOffset int
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
		case "right", "l":
			if maxOff := m.maxLogHOffset(); m.logHOffset < maxOff {
				m.logHOffset = min(m.logHOffset+dashHScrollStep, maxOff)
			}
		case "left", "h":
			m.logHOffset = max(m.logHOffset-dashHScrollStep, 0)
		}
		// A vertical scroll can change which lines are visible (and thus the
		// widest one), so keep the horizontal offset within range to avoid
		// leaving the pane stuck on blank columns.
		m.clampLogHOffset()

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.clampLogHOffset()

	case dashboardLogMsg:
		m.logs = append(m.logs, formatLogLines(msg.service, msg.record)...)
		if m.autoScroll {
			maxOff := len(m.logs) - m.logViewHeight()
			if maxOff < 0 {
				maxOff = 0
			}
			m.logOffset = maxOff
		}
		// Tailing onto shorter lines can shrink the widest visible line.
		m.clampLogHOffset()
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

// dashHScrollStep is how many columns each ←/→ press pans the logs pane. It
// mirrors the bubble-table step so horizontal scrolling feels the same across
// the CLI's TUIs.
const dashHScrollStep = 8

// logWindow returns the slice of log rows currently visible vertically, after
// accounting for the two header rows (title + separator) that sit above the log
// body in the right pane.
func (m dashboardModel) logWindow() []string {
	if len(m.logs) == 0 {
		return nil
	}
	start := m.logOffset
	end := start + m.logViewHeight() - 2 // subtract header rows
	if end > len(m.logs) {
		end = len(m.logs)
	}
	if start > end {
		start = end
	}
	if start < 0 {
		start = 0
	}
	return m.logs[start:end]
}

// visibleLogRows returns the currently visible log rows cropped to the logs
// pane width starting at the horizontal scroll offset. ansi.Cut preserves SGR
// styling across the cut so colored severity prefixes survive panning.
func (m dashboardModel) visibleLogRows() []string {
	lw := m.logsWidth()
	window := m.logWindow()
	rows := make([]string, 0, len(window))
	for _, line := range window {
		rows = append(rows, ansi.Cut(line, m.logHOffset, m.logHOffset+lw))
	}
	return rows
}

// maxLogHOffset is the furthest right the logs pane can pan: the widest visible
// line minus the pane width. Beyond this there is nothing more to reveal.
func (m dashboardModel) maxLogHOffset() int {
	lw := m.logsWidth()
	widest := 0
	for _, line := range m.logWindow() {
		if w := visibleWidth(line); w > widest {
			widest = w
		}
	}
	return max(widest-lw, 0)
}

// clampLogHOffset keeps the horizontal offset within [0, maxLogHOffset] so the
// pane never gets stuck showing only blank columns after the visible content
// narrows (e.g. after a vertical scroll or window resize).
func (m *dashboardModel) clampLogHOffset() {
	if maxOff := m.maxLogHOffset(); m.logHOffset > maxOff {
		m.logHOffset = maxOff
	}
	if m.logHOffset < 0 {
		m.logHOffset = 0
	}
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
		rightLines = append(rightLines, m.visibleLogRows()...)
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
	b.WriteString(dashFooterStyle.Render("q/Ctrl+C exit | ↑/↓ scroll | ←/→ pan logs | G/g end/start"))

	return b.String()
}

// --- Background stream goroutines ---

func (m dashboardModel) runLogStream() {
	infoSev := int32(logspb.SeverityNumber_SEVERITY_NUMBER_INFO)
	req := &agentpb.StreamLogsRequest{MinSeverity: &infoSev}
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

func extractMetricValue(m *metricspb.Metric) (string, time.Time) {
	var pts []*metricspb.NumberDataPoint
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
	case *metricspb.NumberDataPoint_AsDouble:
		return fmt.Sprintf("%g", dp.GetAsDouble()), ts
	case *metricspb.NumberDataPoint_AsInt:
		return fmt.Sprintf("%d", dp.GetAsInt()), ts
	default:
		return "?", ts
	}
}

// sanitizeLogText strips terminal control sequences from untrusted log content
// so they cannot corrupt the dashboard's stdout grid. BubbleTea writes View()
// verbatim to stdout and treats each '\n' as a row boundary it owns; a raw '\r',
// cursor-movement/erase escape (e.g. from a `pulling manifest` spinner), or
// other control byte embedded in a log body would otherwise move the real
// terminal cursor and bleed across the pane separator.
//
// Newlines are preserved so the caller can split a multiline body into separate
// rows. Tabs become spaces; carriage returns, ESC-introduced sequences, and all
// other C0/C1/DEL control bytes are dropped.
//
// The escape grammar is parsed by class rather than "drop until the next ASCII
// letter", so an attacker-controlled OSC payload (e.g. `ESC ] 0 ; title BEL`,
// as emitted by `pulling manifest` style spinners) cannot leak its tail, and a
// two-character escape (e.g. `ESC 7`) cannot swallow the printable text that
// follows it. Raw ESC and all control bytes are dropped unconditionally, so no
// escape or control byte ever reaches stdout regardless of sequence shape.
func sanitizeLogText(s string) string {
	var b strings.Builder
	b.Grow(len(s))

	const (
		stNormal    = iota // ordinary text
		stEscStart         // saw ESC (or an 8-bit C1 introducer); classify next rune
		stCSI              // CSI sequence; ends at a final byte 0x40-0x7e
		stString           // OSC/DCS/PM/APC/SOS string; ends at BEL or ST (ESC \)
		stStringEsc        // saw ESC inside a string; ST iff the next rune is '\'
	)
	state := stNormal

	// Decode runes explicitly rather than ranging: a raw 8-bit C1 control byte
	// (e.g. the 0x9b CSI introducer) is not valid UTF-8, so `range` would yield
	// RuneError and hide it. Unifying the raw byte value with the decoded rune
	// lets the control/introducer checks see C1 controls in either form while
	// still preserving valid multi-byte UTF-8 (e.g. braille spinner glyphs).
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r = rune(s[i]) // invalid byte: treat by its raw value
		}
		i += size

		switch state {
		case stEscStart:
			switch {
			case r == '[':
				state = stCSI
			case r == ']' || r == 'P' || r == 'X' || r == '^' || r == '_':
				state = stString
			default:
				// Two-character escape (ESC 7, ESC =, ESC M, ...): complete.
				state = stNormal
			}
		case stCSI:
			// CSI parameter/intermediate bytes precede a final byte in 0x40-0x7e.
			if r >= 0x40 && r <= 0x7e {
				state = stNormal
			}
		case stString:
			switch r {
			case '\x07': // BEL terminator
				state = stNormal
			case '\x1b': // possible ST (ESC \)
				state = stStringEsc
			}
		case stStringEsc:
			// ST is ESC '\'; otherwise the ESC began a fresh sequence inside the
			// string, so stay in the string and reinterpret the rune there.
			if r == '\\' {
				state = stNormal
			} else {
				state = stString
			}
		default: // stNormal
			switch {
			case r == '\x1b':
				state = stEscStart
			case r == 0x9b: // 8-bit CSI introducer
				state = stCSI
			case r == 0x9d || r == 0x90 || r == 0x9e || r == 0x9f || r == 0x98:
				// 8-bit OSC/DCS/PM/APC/SOS introducers.
				state = stString
			case r == '\n':
				b.WriteRune('\n')
			case r == '\t':
				b.WriteByte(' ')
			case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || unicode.Is(unicode.Cf, r):
				// Drop C0/C1 controls and Unicode format controls (bidi/zero-width).
			default:
				b.WriteRune(r)
			}
		}
	}
	return b.String()
}

// visibleWidth counts rendered columns, skipping ANSI escape sequences.
func visibleWidth(s string) int {
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
	return visible
}

// formatLogLines renders one log record into one or more display rows. A
// multiline body is split into separate rows; continuation rows are indented to
// align under the body so the timestamp/severity prefix stays readable.
// Attributes are appended to the final row.
func formatLogLines(service string, lr *logspb.LogRecord) []string {
	ts := time.Unix(0, int64(lr.GetTimeUnixNano())).Local().Format("15:04:05")
	label, style := severityLabel(lr.GetSeverityNumber())

	var pb strings.Builder
	pb.WriteString(logTimeStyle.Render(ts))
	pb.WriteByte(' ')
	pb.WriteString(style.Render(label))
	if service != "" {
		pb.WriteByte(' ')
		pb.WriteString(logAppStyle.Render("[" + service + "]"))
	}
	pb.WriteByte(' ')
	prefix := pb.String()
	indent := strings.Repeat(" ", visibleWidth(prefix))

	var bodyLines []string
	if body := lr.GetBody(); body != nil {
		bodyLines = strings.Split(sanitizeLogText(body.GetStringValue()), "\n")
		// Drop only the single empty element produced by a terminating '\n'
		// (container output usually ends in one); intentional interior or
		// trailing blank lines are preserved so records render in full.
		if n := len(bodyLines); n > 0 && bodyLines[n-1] == "" {
			bodyLines = bodyLines[:n-1]
		}
	}

	var attrStr string
	if attrs := lr.GetAttributes(); len(attrs) > 0 {
		var ab strings.Builder
		for i, kv := range attrs {
			if i > 0 {
				ab.WriteByte(' ')
			}
			ab.WriteString(logMetaStyle.Render(sanitizeLogText(kv.GetKey()) + "=" + sanitizeLogText(anyValueString(kv.GetValue()))))
		}
		attrStr = ab.String()
	}

	var rows []string
	for i, bl := range bodyLines {
		if i == 0 {
			rows = append(rows, prefix+bl)
		} else {
			rows = append(rows, indent+bl)
		}
	}
	if len(rows) == 0 {
		// No body: keep the prefix (trimmed of its trailing space) on its own row.
		rows = append(rows, strings.TrimRight(prefix, " "))
	}
	if attrStr != "" {
		rows[len(rows)-1] += " " + attrStr
	}
	return rows
}
