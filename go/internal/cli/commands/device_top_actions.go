package commands

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
)

type topStorageTickMsg struct{}
type topStorageMsg struct {
	storage *agentpb.DiskPartition
	err     error
}
type topStartResultMsg struct {
	seq uint64
	app string
	err error
}
type topPruneResultMsg struct {
	seq  uint64
	text string
	err  error
}
type topLogsMsg struct {
	seq      uint64
	stream   grpc.ServerStreamingClient[agentpb.StreamLogsResponse]
	response *agentpb.StreamLogsResponse
	err      error
}

func (m topModel) actionBusy() bool {
	return m.stoppingApp != "" || m.startingApp != "" || m.pruningCache
}

func (m topModel) startAppCmd(app string, seq uint64) tea.Cmd {
	return func() tea.Msg {
		if m.conn == nil || m.conn.ContainerService == nil {
			return topStartResultMsg{seq: seq, app: app, err: fmt.Errorf("container service unavailable")}
		}
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
		defer cancel()
		stream, err := m.conn.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
			AppName:       app,
			RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
		})
		if err == nil {
			err = awaitStarted(stream)
		}
		return topStartResultMsg{seq: seq, app: app, err: err}
	}
}

func fetchTopStorage(ctx context.Context, conn *grpcclient.AgentConnection) (*agentpb.DiskPartition, error) {
	if conn == nil || conn.AgentService == nil {
		return nil, fmt.Errorf("agent service unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return nil, err
	}
	if storage := resp.GetContainerStorage(); storage != nil {
		return storage, nil
	}
	// Older WendyOS agents report /data in the partition list but do not
	// identify container storage separately. Never substitute the root partition.
	for _, partition := range resp.GetPartitions() {
		if partition.GetMountpoint() == "/data" {
			return partition, nil
		}
	}
	return nil, fmt.Errorf("agent does not report container storage")
}

func (m topModel) fetchStorageCmd() tea.Cmd {
	return func() tea.Msg {
		storage, err := fetchTopStorage(m.ctx, m.conn)
		return topStorageMsg{storage: storage, err: err}
	}
}

func (m topModel) pruneCacheCmd(seq uint64) tea.Cmd {
	return func() tea.Msg {
		if m.conn == nil || m.conn.Conn == nil {
			return topPruneResultMsg{seq: seq, err: fmt.Errorf("container service unavailable")}
		}
		ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
		defer cancel()
		var out strings.Builder
		err := runDeviceCachePrune(ctx, m.conn, &out, false, false)
		// The command's second line explains background GC. Keep the interactive
		// status to one line so the meters and table retain their screen space.
		text, _, _ := strings.Cut(strings.TrimSpace(out.String()), "\n")
		return topPruneResultMsg{seq: seq, text: text, err: err}
	}
}

func (m topModel) openLogs() (tea.Model, tea.Cmd) {
	app := m.selectedAppName()
	if app == "" {
		return m, nil
	}
	m.logsSeq++
	seq := m.logsSeq
	ctx, cancel := context.WithCancel(m.ctx)
	m.logsCancel = cancel
	m.logsApp = app
	m.logsLines = nil
	m.logsStatus = "Connecting to logs…"
	m.logsViewport = viewport.New(80, 20)
	m.resizeLogs()
	return m, func() tea.Msg {
		if m.conn == nil || m.conn.TelemetryService == nil {
			return topLogsMsg{seq: seq, err: fmt.Errorf("log service unavailable")}
		}
		lastN := int32(100)
		stream, err := m.conn.TelemetryService.StreamLogs(ctx, &agentpb.StreamLogsRequest{AppName: &app, LastN: &lastN})
		return topLogsMsg{seq: seq, stream: stream, err: err}
	}
}

func receiveTopLogs(seq uint64, stream grpc.ServerStreamingClient[agentpb.StreamLogsResponse]) tea.Cmd {
	return func() tea.Msg {
		response, err := stream.Recv()
		return topLogsMsg{seq: seq, stream: stream, response: response, err: err}
	}
}

func (m topModel) updateLogs(msg topLogsMsg) (tea.Model, tea.Cmd) {
	if m.logsApp == "" || msg.seq != m.logsSeq {
		return m, nil
	}
	if msg.err != nil {
		if msg.err == io.EOF {
			m.logsStatus = "Log stream ended"
		} else {
			m.logsStatus = "Logs unavailable: " + userFacingGRPCError(msg.err)
		}
		if m.logsCancel != nil {
			m.logsCancel()
			m.logsCancel = nil
		}
		return m, nil
	}
	m.logsStatus = "Following logs"
	atBottom := m.logsViewport.AtBottom()
	if response := msg.response; response != nil {
		var text strings.Builder
		for _, resource := range response.GetLogs().GetResourceLogs() {
			service := resourceServiceName(resource.GetResource())
			for _, scope := range resource.GetScopeLogs() {
				for _, record := range scope.GetLogRecords() {
					for _, line := range formatLogLines(sanitizeLogText(service), record) {
						text.WriteString(line)
						text.WriteByte('\n')
					}
				}
			}
		}
		if text.Len() > 0 {
			m.logsLines = append(m.logsLines, strings.Split(strings.TrimSuffix(text.String(), "\n"), "\n")...)
			const maxLogLines = 2000
			if len(m.logsLines) > maxLogLines {
				dropped := len(m.logsLines) - maxLogLines
				m.logsLines = append([]string(nil), m.logsLines[dropped:]...)
				m.logsViewport.SetYOffset(max(0, m.logsViewport.YOffset-dropped))
			}
			m.logsViewport.SetContent(strings.Join(m.logsLines, "\n"))
			if atBottom {
				m.logsViewport.GotoBottom()
			}
		}
	}
	return m, receiveTopLogs(msg.seq, msg.stream)
}

func (m *topModel) resizeLogs() {
	width, height := m.width, m.height
	if width <= 0 {
		width = 80
	}
	if height <= 0 {
		height = 24
	}
	m.logsViewport.Width = width
	m.logsViewport.Height = max(1, height-3)
}

func (m topModel) updateLogsKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		if m.logsCancel != nil {
			m.logsCancel()
		}
		m.logsCancel = nil
		m.logsApp = ""
		m.logsLines = nil
		return m, nil
	case "q", "ctrl+c":
		if m.logsCancel != nil {
			m.logsCancel()
		}
		return m, tea.Quit
	case "end", "G":
		m.logsViewport.GotoBottom()
		return m, nil
	}
	var cmd tea.Cmd
	m.logsViewport, cmd = m.logsViewport.Update(msg)
	return m, cmd
}

func (m topModel) logsView() string {
	width := m.logsViewport.Width
	title := topHeaderBar.Render(padOrCrop(" Logs: "+tui.StripControl(m.logsApp), width))
	status := topValDim.Render(padOrCrop(" "+tui.StripControl(m.logsStatus), width))
	footer := topValDim.Render(padOrCrop(" esc back · ↑/↓ scroll · pgup/pgdown page · end follow · q quit", width))
	return strings.Join([]string{title, status, m.logsViewport.View(), footer}, "\n")
}
