package chat

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

type backgroundStatusMCP struct {
	result   *mcpgo.CallToolResult
	err      error
	calls    []mcpgo.CallToolRequest
	deadline time.Time
	tools    []mcpgo.Tool
}

func (m *backgroundStatusMCP) ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	return &mcpgo.ListToolsResult{Tools: m.tools}, nil
}

func (m *backgroundStatusMCP) CallTool(ctx context.Context, request mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	m.calls = append(m.calls, request)
	m.deadline, _ = ctx.Deadline()
	return m.result, m.err
}

func (*backgroundStatusMCP) Close() error { return nil }

func TestBackgroundArgsKeepExactDeviceAndOptions(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		target     backgroundTarget
		arguments  string
		want       []string
	}{
		{
			name: "camera_zero_id_and_custom_port", kind: "camera_view",
			target:    backgroundTarget{Device: "woof.local:51234", Transport: "direct"},
			arguments: `{"camera_id":0,"width":1920,"height":1080,"fps":30}`,
			want:      []string{"--device=woof.local:51234", "device", "camera", "view", "--non-interactive", "--id=0", "--width=1920", "--height=1080", "--fps=30"},
		},
		{
			name: "camera_default_selection", kind: "camera_view",
			target: backgroundTarget{Device: "vm:robot-lab", Transport: "direct"}, arguments: `{}`,
			want: []string{"--device=vm:robot-lab", "device", "camera", "view", "--non-interactive"},
		},
		{
			name: "audio_integer_exponents", kind: "audio_listen",
			target:    backgroundTarget{Device: "[2001:db8::1]:50052", Transport: "direct"},
			arguments: `{"device_id":0,"sample_rate":1.6e4,"channels":2.0,"buffer_ms":1.5e2}`,
			want:      []string{"--device=[2001:db8::1]:50052", "device", "audio", "listen", "--non-interactive", "--id=0", "--sample-rate=16000", "--channels=2", "--buffer-ms=150"},
		},
		{
			name: "cloud_camera_custom_endpoints", kind: "camera_view",
			target:    backgroundTarget{Device: "Selected Woof", Transport: "cloud", CloudGRPC: "private-cloud.example:5443", BrokerURL: "private-broker.example:5444"},
			arguments: `{"stable_id":"front camera","width":640,"height":480,"fps":15}`,
			want:      []string{"--device=Selected Woof", "cloud", "device", "--cloud-grpc=private-cloud.example:5443", "--broker-url=private-broker.example:5444", "camera", "view", "--non-interactive", "--stable-id=front camera", "--width=640", "--height=480", "--fps=15"},
		},
		{
			name: "cloud_audio_default_broker", kind: "audio_listen",
			target:    backgroundTarget{Device: "Woof", Transport: "cloud", CloudGRPC: "cloud.example:443"},
			arguments: `{"device_id":4294967295,"sample_rate":48000,"channels":1,"buffer_ms":250}`,
			want:      []string{"--device=Woof", "cloud", "device", "--cloud-grpc=cloud.example:443", "--broker-url=", "audio", "listen", "--non-interactive", "--id=4294967295", "--sample-rate=48000", "--channels=1", "--buffer-ms=250"},
		},
		{
			name: "identifiers_are_single_literal_arguments", kind: "camera_view",
			target:    backgroundTarget{Device: "Woof $(echo nope); name", Transport: "direct"},
			arguments: "{\"stable_id\":\"-front; $(touch marker) `echo x` * $HOME\"}",
			want:      []string{"--device=Woof $(echo nope); name", "device", "camera", "view", "--non-interactive", "--stable-id=-front; $(touch marker) `echo x` * $HOME"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, err := backgroundArgs(tc.kind, tc.target, json.RawMessage(tc.arguments))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(args, tc.want) {
				t.Fatalf("arguments = %#v, want %#v", args, tc.want)
			}
		})
	}
}

func TestBackgroundArgsRejectInvalidIdentifiersAndNumbers(t *testing.T) {
	target := backgroundTarget{Device: "woof.local:51234", Transport: "direct"}
	for _, arguments := range []string{
		`{"camera_id":0,"stable_id":"front"}`, `{"stable_id":""}`, `{"stable_id":"front\nview"}`,
		`{"stable_id":"front\u0000view"}`, `{"stable_id":7}`, `{"camera_id":-1}`,
		`{"camera_id":1.5}`, `{"camera_id":4294967296}`, `{"camera_id":null}`, `{"camera_id":true}`,
	} {
		t.Run(arguments, func(t *testing.T) {
			if _, err := backgroundArgs("camera_view", target, json.RawMessage(arguments)); err == nil {
				t.Fatalf("accepted invalid camera arguments %s", arguments)
			}
		})
	}
	if _, err := backgroundArgs("workspace_exec", target, json.RawMessage(`{}`)); err == nil {
		t.Fatal("background command accepted an arbitrary tool")
	}
}

func TestBackgroundTargetValidation(t *testing.T) {
	for _, target := range []backgroundTarget{
		{Transport: "direct"}, {Device: "--help", Transport: "direct"},
		{Device: " woof", Transport: "direct"}, {Device: "woof\n", Transport: "direct"},
		{Device: strings.Repeat("x", 513), Transport: "direct"}, {Device: "woof", Transport: "ssh"},
		{Device: "woof", Transport: "direct", CloudGRPC: "cloud.example:443"},
		{Device: "woof", Transport: "direct", BrokerURL: "broker.example:443"},
		{Device: "woof", Transport: "cloud"},
		{Device: "woof", Transport: "cloud", CloudGRPC: "cloud.example:443\u0000"},
		{Device: "woof", Transport: "cloud", CloudGRPC: "cloud.example:443", BrokerURL: " broker.example:443"},
	} {
		if _, err := backgroundArgs("camera_view", target, json.RawMessage(`{}`)); err == nil {
			t.Errorf("accepted invalid target %+v", target)
		}
	}
}

func TestBackgroundTargetReadsStructuredAndTextStatus(t *testing.T) {
	status := `{"connected":true,"command_target":{"device":"Selected Woof","transport":"cloud","cloud_grpc":"private-cloud.example:5443","broker_url":"private-broker.example:5444"}}`
	var structured any
	if err := json.Unmarshal([]byte(status), &structured); err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]*mcpgo.CallToolResult{
		"structured":   {StructuredContent: structured, Content: []mcpgo.Content{mcpgo.TextContent{Type: "text", Text: "ignored prose"}}},
		"text":         mcpgo.NewToolResultText(status),
		"text_pointer": {Content: []mcpgo.Content{&mcpgo.TextContent{Type: "text", Text: status}}},
	} {
		t.Run(name, func(t *testing.T) {
			tools := testWorkspaceTools(t)
			client := &backgroundStatusMCP{result: result}
			tools.mcp = client
			target, err := tools.backgroundTarget(context.Background())
			want := backgroundTarget{Device: "Selected Woof", Transport: "cloud", CloudGRPC: "private-cloud.example:5443", BrokerURL: "private-broker.example:5444"}
			if err != nil || target != want {
				t.Fatalf("target = %+v, %v; want %+v", target, err, want)
			}
			if len(client.calls) != 1 || client.calls[0].Params.Name != "wendy_status" {
				t.Fatalf("expected a status lookup, got %+v", client.calls)
			}
			if client.deadline.IsZero() || time.Until(client.deadline) > 5*time.Second {
				t.Fatalf("status lookup deadline = %v", client.deadline)
			}
		})
	}
}

func TestBackgroundTargetRequiresCurrentReplayableConnection(t *testing.T) {
	tools := testWorkspaceTools(t)
	if _, err := tools.backgroundTarget(context.Background()); err == nil {
		t.Fatal("accepted playback without an MCP client")
	}
	client := &backgroundStatusMCP{}
	tools.mcp = client
	for _, result := range []*mcpgo.CallToolResult{
		nil, mcpgo.NewToolResultError("offline"), mcpgo.NewToolResultText("not JSON"),
		mcpgo.NewToolResultText(`{"connected":false,"command_target":{"device":"stale-woof:50051","transport":"direct"}}`),
		mcpgo.NewToolResultText(`{"connected":true,"device":"friendly-name-without-an-endpoint","connection_type":"direct"}`),
		mcpgo.NewToolResultText(`{"connected":true,"command_target":{"device":"Woof","transport":"cloud"}}`),
	} {
		client.result = result
		if _, err := tools.backgroundTarget(context.Background()); err == nil {
			t.Fatalf("accepted missing/invalid connection: %+v", result)
		}
	}
	client.err = errors.New("transport failed")
	if _, err := tools.backgroundTarget(context.Background()); err == nil || !strings.Contains(err.Error(), "transport failed") {
		t.Fatalf("status transport error = %v", err)
	}
}

func TestBackgroundTargetDoesNotReuseInitialOrPreviousDevice(t *testing.T) {
	tools := testWorkspaceTools(t)
	client := &backgroundStatusMCP{}
	tools.mcp = client
	for _, tc := range []struct {
		status string
		want   backgroundTarget
	}{
		{`{"connected":true,"command_target":{"device":"initial-woof:51234","transport":"direct"}}`, backgroundTarget{Device: "initial-woof:51234", Transport: "direct"}},
		{`{"connected":true,"command_target":{"device":"New Woof","transport":"cloud","cloud_grpc":"new-cloud.example:443"}}`, backgroundTarget{Device: "New Woof", Transport: "cloud", CloudGRPC: "new-cloud.example:443"}},
		{`{"connected":false}`, backgroundTarget{}},
	} {
		client.result = mcpgo.NewToolResultText(tc.status)
		got, err := tools.backgroundTarget(context.Background())
		if got != tc.want || (err != nil) != (tc.want.Device == "") {
			t.Fatalf("target = %+v, %v; want %+v", got, err, tc.want)
		}
	}
	if len(client.calls) != 3 {
		t.Fatalf("status lookups = %d, want one for every target resolution", len(client.calls))
	}
}

func TestBackgroundToolsRemainAvailableAfterDynamicRefresh(t *testing.T) {
	tools := testWorkspaceTools(t)
	client := &backgroundStatusMCP{}
	tools.mcp = client
	for _, remote := range []string{"original_sensor", "new_sensor"} {
		client.tools = []mcpgo.Tool{mcpgo.NewTool(remote)}
		listing, err := tools.ListTools(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		byName := make(map[string]Tool)
		for _, tool := range listing {
			byName[tool.Name] = tool
		}
		for name, approval := range map[string]bool{"camera_view": true, "audio_listen": true, "background_process_list": false, "background_process_stop": false} {
			tool, ok := byName[name]
			if !ok || tool.RequiresApproval != approval {
				t.Fatalf("missing tool or wrong approval metadata for %s: %+v", name, tool)
			}
		}
		if _, ok := byName[remote]; !ok {
			t.Fatalf("missing current remote tool %s", remote)
		}
	}
}

func TestBackgroundToolSchemasValidateBeforeStatusOrSpawn(t *testing.T) {
	tools := testWorkspaceTools(t)
	client := &backgroundStatusMCP{}
	tools.mcp = client
	for _, tc := range []struct{ name, arguments string }{
		{"camera_view", `{"camera_id":-1}`}, {"camera_view", `{"camera_id":4294967296}`},
		{"camera_view", `{"stable_id":""}`}, {"camera_view", `{"stable_id":"` + strings.Repeat("x", 256) + `"}`},
		{"camera_view", `{"width":8193}`}, {"camera_view", `{"height":-1}`}, {"camera_view", `{"fps":241}`},
		{"camera_view", `{"command":"touch marker"}`}, {"camera_view", `{"camera_id":"0"}`},
		{"audio_listen", `{"device_id":-1}`}, {"audio_listen", `{"sample_rate":7999}`}, {"audio_listen", `{"sample_rate":192001}`},
		{"audio_listen", `{"channels":0}`}, {"audio_listen", `{"channels":3}`},
		{"audio_listen", `{"buffer_ms":19}`}, {"audio_listen", `{"buffer_ms":2001}`},
		{"audio_listen", `{"sample_rate":16000.5}`}, {"audio_listen", `{"device_id":null}`},
		{"background_process_list", `{"job_id":""}`}, {"background_process_list", `{"job_id":"` + strings.Repeat("x", 65) + `"}`},
		{"background_process_stop", `{}`}, {"background_process_stop", `{"pid":12345}`},
	} {
		t.Run(tc.name+tc.arguments, func(t *testing.T) {
			_, err := tools.Execute(context.Background(), ToolCall{Name: tc.name, Arguments: json.RawMessage(tc.arguments)})
			if err == nil || !strings.Contains(err.Error(), "invalid arguments") {
				t.Fatalf("schema did not reject %s: %v", tc.arguments, err)
			}
		})
	}
	if len(client.calls) != 0 {
		t.Fatalf("invalid arguments called MCP: %+v", client.calls)
	}
	for _, tc := range []struct{ name, arguments string }{
		{"camera_view", `{}`}, {"camera_view", `{"camera_id":0,"width":0,"height":8192,"fps":240}`},
		{"audio_listen", `{}`}, {"audio_listen", `{"device_id":4294967295,"sample_rate":1.6e4,"channels":2.0,"buffer_ms":150}`},
		{"background_process_list", `{}`}, {"background_process_stop", `{"job_id":"media-1"}`},
	} {
		if err := validateArguments(tools.known[tc.name], json.RawMessage(tc.arguments)); err != nil {
			t.Errorf("valid arguments rejected for %s: %v", tc.name, err)
		}
	}
}

func TestBackgroundStartsRequireApprovalAndListStopStayLocal(t *testing.T) {
	tools := testWorkspaceTools(t)
	client := &backgroundStatusMCP{}
	tools.mcp = client
	tools.background = &backgroundProcesses{workspace: tools.workspace}
	var approvals []string
	round := 0
	provider := uiProviderFunc(func(context.Context, []Message, []Tool, func(string)) (Message, error) {
		round++
		if round == 1 {
			return Message{ToolCalls: []ToolCall{
				{ID: "camera", Name: "camera_view", Arguments: json.RawMessage(`{}`)},
				{ID: "audio", Name: "audio_listen", Arguments: json.RawMessage(`{}`)},
				{ID: "list", Name: "background_process_list", Arguments: json.RawMessage(`{}`)},
				{ID: "stop", Name: "background_process_stop", Arguments: json.RawMessage(`{"job_id":"media-unknown"}`)},
			}}, nil
		}
		return Message{Content: "Done."}, nil
	})
	engine := NewEngine(provider, tools, "test")
	if err := engine.Turn(context.Background(), "Show and manage playback.", func(Event) {}, func(_ context.Context, call ToolCall) (bool, error) {
		approvals = append(approvals, call.Name)
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(approvals, []string{"camera_view", "audio_listen"}) {
		t.Fatalf("approval requests = %v", approvals)
	}
	if len(client.calls) != 0 {
		t.Fatalf("denied starts or local management called MCP: %+v", client.calls)
	}
	results := map[string]string{}
	for _, message := range engine.Messages() {
		if message.Role == "tool" {
			results[message.ToolCallID] = message.Content
		}
	}
	if !strings.Contains(results["camera"], "denied") || !strings.Contains(results["audio"], "denied") {
		t.Fatalf("starts were not denied: %+v", results)
	}
	if results["list"] != `{"jobs":[]}` || !strings.Contains(results["stop"], "only this chat's jobs") {
		t.Fatalf("local management results = %+v", results)
	}
}
