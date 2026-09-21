package commands

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc"
)

type topStartClient struct {
	agentpb.WendyContainerServiceClient
	request *agentpb.StartContainerRequest
	stream  *topStartStream
}

func (f *topStartClient) StartContainer(_ context.Context, req *agentpb.StartContainerRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	f.request = req
	return f.stream, nil
}

type topStartStream struct {
	grpc.ClientStream
	responses []*agentpb.RunContainerLayersResponse
}

func (s *topStartStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	if len(s.responses) == 0 {
		return nil, io.EOF
	}
	response := s.responses[0]
	s.responses = s.responses[1:]
	return response, nil
}

func TestTopStartWaitsForConfirmation(t *testing.T) {
	for _, confirmed := range []bool{false, true} {
		t.Run(map[bool]string{true: "confirmed", false: "closed early"}[confirmed], func(t *testing.T) {
			stream := &topStartStream{}
			if confirmed {
				stream.responses = []*agentpb.RunContainerLayersResponse{{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}}
			}
			client := &topStartClient{stream: stream}
			m := newTopModel(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, time.Second)
			m.rows = []topRow{{name: "app", state: agentpb.AppRunningState_STOPPED}}
			model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("s")})
			m = model.(topModel)
			if m.startingApp != "app" || cmd == nil {
				t.Fatal("start key did not initiate app start")
			}
			// A lifecycle request in progress cannot be overwritten by another action.
			_, duplicate := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
			if duplicate != nil {
				t.Fatal("cache action overlapped app start")
			}
			result := cmd().(topStartResultMsg)
			if (result.err == nil) != confirmed {
				t.Fatalf("start err = %v, confirmed = %v", result.err, confirmed)
			}
			if client.request.GetAppName() != "app" || client.request.GetRestartPolicy().GetMode() != agentpb.RestartPolicyMode_UNLESS_STOPPED {
				t.Fatalf("request = %v", client.request)
			}
			model, _ = m.Update(result)
			m = model.(topModel)
			if m.startingApp != "" {
				t.Fatal("start remained busy after completion")
			}
		})
	}
}

type topLogClient struct {
	agentpb.WendyTelemetryServiceClient
	request *agentpb.StreamLogsRequest
	ctx     context.Context
	stream  *topLogStream
}

func (f *topLogClient) StreamLogs(ctx context.Context, req *agentpb.StreamLogsRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.StreamLogsResponse], error) {
	f.request, f.ctx = req, ctx
	return f.stream, nil
}

type topLogStream struct{ grpc.ClientStream }

func (*topLogStream) Recv() (*agentpb.StreamLogsResponse, error) { return nil, io.EOF }

func TestTopLogsEscapeCancelsAndReturnsToSelection(t *testing.T) {
	client := &topLogClient{stream: &topLogStream{}}
	m := newTopModel(context.Background(), &grpcclient.AgentConnection{TelemetryService: client}, time.Second)
	m.rows = []topRow{{name: "app", state: agentpb.AppRunningState_RUNNING}, {isSubrow: true}}
	m.cursor = 1
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(topModel)
	if m.logsApp != "app" {
		t.Fatalf("logs app = %q", m.logsApp)
	}
	opened := cmd().(topLogsMsg)
	if client.request.GetAppName() != "app" || client.request.GetLastN() != 100 {
		t.Fatalf("request = %v", client.request)
	}
	model, receive := m.Update(opened)
	m = model.(topModel)
	if receive == nil || !strings.Contains(m.View(), "esc back") {
		t.Fatal("logs view did not start receiving")
	}
	model, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = model.(topModel)
	if cmd != nil || m.logsApp != "" || m.cursor != 1 {
		t.Fatal("escape did not return to selected row")
	}
	if !errors.Is(client.ctx.Err(), context.Canceled) {
		t.Fatal("escape did not cancel stream")
	}
	model, cmd = m.Update(opened)
	if cmd != nil || model.(topModel).logsApp != "" {
		t.Fatal("stale stream response reopened logs")
	}
	model, openAgain := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = model.(topModel)
	newer := openAgain().(topLogsMsg)
	model, cmd = m.Update(opened)
	m = model.(topModel)
	if cmd != nil || m.logsSeq == opened.seq || m.logsStatus != "Connecting to logs…" {
		t.Fatal("old stream result affected a reopened log view")
	}
	model, _ = m.Update(newer)
	m = model.(topModel)
	_, quit := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if quit == nil || !errors.Is(client.ctx.Err(), context.Canceled) {
		t.Fatal("quit did not cancel log stream")
	}
}

func TestTopLogsBufferIsBoundedAndResizes(t *testing.T) {
	m := newTopModel(context.Background(), nil, time.Second)
	m.rows = []topRow{{name: "app"}}
	model, _ := m.openLogs()
	m = model.(topModel)
	defer m.logsCancel()
	body := strings.Repeat("line\n", 2100)
	response := &agentpb.StreamLogsResponse{Logs: &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}}}}}}}}}}
	model, _ = m.Update(topLogsMsg{seq: m.logsSeq, stream: &topLogStream{}, response: response})
	m = model.(topModel)
	if len(m.logsLines) != 2000 || !m.logsViewport.AtBottom() {
		t.Fatalf("lines=%d, atBottom=%v", len(m.logsLines), m.logsViewport.AtBottom())
	}
	model, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m = model.(topModel)
	if m.logsViewport.Width != 100 || m.logsViewport.Height != 27 {
		t.Fatalf("viewport size = %dx%d", m.logsViewport.Width, m.logsViewport.Height)
	}
}

type topStorageClient struct {
	agentpb.WendyAgentServiceClient
	response *agentpb.GetAgentVersionResponse
}

func (f *topStorageClient) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	return f.response, nil
}

func TestTopStorageUsesContainerPartition(t *testing.T) {
	root := &agentpb.DiskPartition{Mountpoint: "/", UsedBytes: 1, TotalBytes: 10}
	data := &agentpb.DiskPartition{Mountpoint: "/data", UsedBytes: 5, TotalBytes: 20}
	custom := &agentpb.DiskPartition{Mountpoint: "/containers", UsedBytes: 7, TotalBytes: 30}
	for _, tt := range []struct {
		name     string
		response *agentpb.GetAgentVersionResponse
		want     *agentpb.DiskPartition
	}{
		{"explicit", &agentpb.GetAgentVersionResponse{ContainerStorage: custom, Partitions: []*agentpb.DiskPartition{root, data}}, custom},
		{"older agent data", &agentpb.GetAgentVersionResponse{Partitions: []*agentpb.DiskPartition{root, data}}, data},
		{"root is not container storage", &agentpb.GetAgentVersionResponse{Partitions: []*agentpb.DiskPartition{root}}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fetchTopStorage(context.Background(), &grpcclient.AgentConnection{AgentService: &topStorageClient{response: tt.response}})
			if got != tt.want || (err != nil) != (tt.want == nil) {
				t.Fatalf("storage=%v, error=%v, want %v", got, err, tt.want)
			}
		})
	}
}

func TestTopStorageShownInDashboardAndSnapshots(t *testing.T) {
	storage := &agentpb.DiskPartition{Mountpoint: "/data", UsedBytes: 1 << 30, TotalBytes: 2 << 30}
	m := newTopModel(context.Background(), nil, time.Second)
	model, _ := m.Update(topStorageMsg{storage: storage})
	m = model.(topModel)
	if !strings.Contains(m.View(), "Disk /data") {
		t.Fatalf("missing disk meter: %s", m.View())
	}
	model, _ = m.Update(topStorageMsg{err: errors.New("offline")})
	if !strings.Contains(model.(topModel).View(), "(stale)") {
		t.Fatal("failed refresh was not marked stale")
	}
	sample := topSample{storage: storage}
	output := buildTopJSON(topSample{}, sample, nil)
	if output.Host.ContainerStorage == nil || output.Host.ContainerStorage.Mountpoint != "/data" {
		t.Fatalf("missing JSON storage: %+v", output)
	}
	var out bytes.Buffer
	if err := writeTopPlainSnapshot(&out, topSample{}, sample, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "DISK /data:") {
		t.Fatalf("missing plain storage: %s", out.String())
	}
}

func TestTopPruneKeyAndCompletion(t *testing.T) {
	m := newTopModel(context.Background(), nil, time.Second)
	model, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	m = model.(topModel)
	if cmd == nil || !m.pruningCache {
		t.Fatal("cache prune key did not start action")
	}
	model, _ = m.Update(topPruneResultMsg{seq: m.actionSeq, text: "Released: 1 GiB"})
	m = model.(topModel)
	if m.pruningCache || m.actionStatus != "Released: 1 GiB" {
		t.Fatalf("prune did not complete: %s", m.actionStatus)
	}
}

func TestTopLogsSanitizeRemoteTerminalControls(t *testing.T) {
	m := newTopModel(context.Background(), nil, time.Second)
	m.rows = []topRow{{name: "app"}}
	model, _ := m.openLogs()
	m = model.(topModel)
	defer m.logsCancel()
	bad := "visible\rforged\x1b[2J\x1b]52;c;secret\a\u202e\u200b"
	value := func(s string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: s}}
	}
	response := &agentpb.StreamLogsResponse{Logs: &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{Body: value(bad), Attributes: []*commonpb.KeyValue{{Key: bad, Value: value(bad)}}}}}}}}}}
	model, _ = m.Update(topLogsMsg{seq: m.logsSeq, stream: &topLogStream{}, response: response})
	rendered := strings.Join(model.(topModel).logsLines, "\n")
	for _, control := range []string{"\r", "\x1b[2J", "\x1b]", "\a", "\u202e", "\u200b"} {
		if strings.Contains(rendered, control) {
			t.Fatalf("remote terminal control survived: %q", rendered)
		}
	}
	if !strings.Contains(rendered, "visible") {
		t.Fatal("discarded printable log content")
	}
}

func TestTopLogsSanitizeTitleAndStatus(t *testing.T) {
	m := newTopModel(context.Background(), nil, time.Second)
	m.rows = []topRow{{name: "robot\x1b[2J\r\n\u202eforged"}}
	model, _ := m.openLogs()
	m = model.(topModel)
	defer m.logsCancel()
	m.logsStatus = "error\x1b]52;c;secret\a\r\n\u202eforged"
	rendered := m.logsView()
	for _, control := range []string{"\x1b[2J", "\x1b]", "\r", "\a", "\u202e"} {
		if strings.Contains(rendered, control) {
			t.Fatalf("remote terminal control survived: %q", rendered)
		}
	}
	if !strings.Contains(rendered, "robot") || !strings.Contains(rendered, "error") {
		t.Fatal("discarded readable title/status")
	}
}
