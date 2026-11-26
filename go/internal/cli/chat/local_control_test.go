package chat

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func controlToolNames(t *testing.T, executor Executor) map[string]Tool {
	t.Helper()
	tools, err := executor.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]Tool{}
	for _, tool := range tools {
		names[tool.Name] = tool
	}
	return names
}

func controlDiscover(t *testing.T, executor Executor, args string) string {
	t.Helper()
	text, err := executor.Execute(context.Background(), ToolCall{Name: "device_tools", Arguments: json.RawMessage(args)})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestLocalControlDiscoveryPreservesPoliciesAndDynamicCapabilities(t *testing.T) {
	ctx := context.Background()
	profile, _ := ResolveProfile("device-control")
	parent, _ := ResolveProfile("device-control")
	// A narrower parent may delegate the device-control profile, but cannot
	// give it capabilities the parent did not have.
	parent.groups = []string{"sensors", "app"}
	schema := json.RawMessage(`{"type":"object","properties":{"limit":{"type":"integer","minimum":1}},"required":["limit"],"additionalProperties":false}`)
	base := &engineTestExecutor{tools: []Tool{
		{Name: "robot_status", Parameters: json.RawMessage(`{"type":"object"}`)},
		{Name: "camera_list", Description: "Read cameras", Parameters: schema},
		{Name: "robot_command", RequiresApproval: true},
		{Name: "telemetry_logs"}, // allowed by child, denied by parent
		{Name: "workspace_exec"}, // denied by child
	}}
	policy := &ProfileTools{Base: base, Profile: profile, Parent: &parent}
	tools := &localControlTools{base: &serviceTools{base: policy, allowed: []string{}}}
	initial := controlToolNames(t, tools)
	if len(initial) != 2 || initial["robot_status"].Name == "" || initial["device_tools"].Name == "" {
		t.Fatalf("initial catalog: %+v", initial)
	}
	catalog := controlDiscover(t, tools, `{}`)
	for _, denied := range []string{"robot_command", "telemetry_logs", "workspace_exec"} {
		if strings.Contains(catalog, denied) {
			t.Fatalf("discovery exposed denied tool %s", denied)
		}
		if _, err := tools.Execute(ctx, ToolCall{Name: "device_tools", Arguments: json.RawMessage(`{"names":["` + denied + `"]}`)}); err == nil {
			t.Fatalf("enabled denied tool %s", denied)
		}
	}
	call := ToolCall{Name: "camera_list", Arguments: json.RawMessage(`{"limit":1}`)}
	if _, err := tools.Execute(ctx, call); err == nil {
		t.Fatal("hidden tool executed directly")
	}
	if _, err := tools.Execute(ctx, ToolCall{Name: "device_tools", Arguments: json.RawMessage(`{"names":["camera_list","not_present"]}`)}); err == nil {
		t.Fatal("accepted partial invalid selection")
	}
	if _, ok := controlToolNames(t, tools)["camera_list"]; ok {
		t.Fatal("invalid selection partly changed visibility")
	}
	controlDiscover(t, tools, `{"names":["camera_list"]}`)
	if got := controlToolNames(t, tools)["camera_list"]; !reflect.DeepEqual(got, base.tools[1]) {
		t.Fatalf("discovery changed schema or policy: %+v", got)
	}
	if _, err := tools.Execute(ctx, ToolCall{Name: "camera_list", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("enabled tool bypassed argument validation")
	}
	if _, err := tools.Execute(ctx, call); err != nil {
		t.Fatal(err)
	}
	base.tools = append(base.tools, Tool{Name: "rover__inspect", Description: "Inspect rover app", Parameters: json.RawMessage(`{"type":"object"}`)})
	if !strings.Contains(controlDiscover(t, tools, `{"query":"rover"}`), "rover__inspect") {
		t.Fatal("new app capability not discovered")
	}
	controlDiscover(t, tools, `{"names":["rover__inspect"]}`)
	if _, err := tools.Execute(ctx, ToolCall{Name: "rover__inspect", Arguments: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	base.tools = base.tools[:1] // reconnect removed the previous capabilities
	if _, err := tools.Execute(ctx, call); err == nil {
		t.Fatal("stale revealed tool still executed")
	}
	if len(base.calls) != 2 {
		t.Fatalf("rejected calls reached underlying executor: %+v", base.calls)
	}
}

func TestLocalControlDiscoveryValidationAndCancellation(t *testing.T) {
	base := &engineTestExecutor{}
	tools := &localControlTools{base: base}
	for _, raw := range []string{`null`, `{"names":"robot_status"}`, `{"names":["x","x"]}`, `{"shell":"do it"}`} {
		if _, err := tools.Execute(context.Background(), ToolCall{Name: "device_tools", Arguments: json.RawMessage(raw)}); err == nil {
			t.Fatalf("accepted invalid discovery: %s", raw)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tools.Execute(ctx, ToolCall{Name: "device_tools", Arguments: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("canceled discovery ran")
	}
	if base.lists != 0 {
		t.Fatal("invalid discovery reached the underlying tools")
	}
}

type controlMediaExecutor struct {
	*engineTestExecutor
	images []Image
}

func (e *controlMediaExecutor) ExecuteResult(ctx context.Context, call ToolCall) (ToolResult, error) {
	text, err := e.Execute(ctx, call)
	return ToolResult{Text: text, Images: e.images}, err
}

func TestLocalControlPreservesCameraImagesAndMutationApproval(t *testing.T) {
	base := &controlMediaExecutor{engineTestExecutor: &engineTestExecutor{tools: []Tool{{Name: "camera_snapshot", RequiresApproval: true, Parameters: json.RawMessage(`{"type":"object"}`)}}}, images: []Image{{MIMEType: "image/jpeg", Data: "image-bytes"}}}
	tools := &localControlTools{base: base}
	controlDiscover(t, tools, `{"names":["camera_snapshot"]}`)
	if !controlToolNames(t, tools)["camera_snapshot"].RequiresApproval {
		t.Fatal("discovery stripped camera approval")
	}
	result, err := tools.ExecuteResult(context.Background(), ToolCall{Name: "camera_snapshot", Arguments: json.RawMessage(`{}`)})
	if err != nil || !reflect.DeepEqual(result.Images, base.images) {
		t.Fatalf("camera media lost: %+v %v", result, err)
	}
	base.calls = nil
	round := 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
		round++
		if round == 1 {
			return Message{ToolCalls: []ToolCall{{Name: "camera_snapshot", Arguments: json.RawMessage(`{}`)}}}, nil
		}
		if !strings.Contains(messages[len(messages)-1].Content, "denied permission") {
			t.Fatal("approval denial not reported")
		}
		return Message{Content: "Stopped."}, nil
	})
	if err := NewEngine(provider, tools, "test").Turn(context.Background(), "snapshot", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(base.calls) != 0 {
		t.Fatal("revealing a mutation granted approval")
	}
}

func TestLocalControlSessionScopeAndPrompt(t *testing.T) {
	for _, provider := range []string{"local", "ollama", "openai", "anthropic"} {
		for _, profileName := range []string{"device-control", "general", "device-sensors"} {
			t.Run(provider+"/"+profileName, func(t *testing.T) {
				profile, _ := ResolveProfile(profileName)
				opts := SessionOptions{Config: Config{Provider: provider}, Profile: profile, Workspace: t.TempDir(), MemoryDirectory: t.TempDir(), SystemInstructions: "Additional user scope", ApprovedTools: []string{}}
				engine, err := sessionEngine(nil, &engineTestExecutor{}, opts)
				if err != nil {
					t.Fatal(err)
				}
				compact := profileName == "device-control" && (provider == "local" || provider == "ollama")
				_, wrapped := engine.memory.base.(*localControlTools)
				if wrapped != compact || engine.skipAutomaticLearning != compact {
					t.Fatal("compact mode applied to wrong session")
				}
				if !strings.Contains(engine.system, opts.SystemInstructions) || !strings.Contains(engine.system, "unattended service") {
					t.Fatal("session instructions lost")
				}
				if !engine.MemoryEnabled() {
					t.Fatal("compact mode disabled memory")
				}
				if !compact && !strings.HasPrefix(engine.system, profile.Prompt(opts.Workspace, opts.Device)) {
					t.Fatal("other profile/provider prompt changed")
				}
			})
		}
	}
	prompt := localControlPrompt("/project", "my-device")
	if len(prompt) > 6000 {
		t.Fatalf("compact prompt grew to %d bytes", len(prompt))
	}
	for _, guard := range []string{"robot_camera_snapshot immediately", "call robot_command action connect", "Do not ask the user to type tool syntax", "Never evade a denial", "Never bypass obstacle", "navigable evidence_id", "Never supply request_id", "completed receipt", "skips the extra post-turn model review", "source_call_ids", "robot_command action localize", "Never translate alignment into turn", "Never invent a serial", "ReadinessMessage", "LastMotionFailure", "do not end with a promise to retry", "IdleMeasurementsReady", "IdleReadinessMessage", "They do not permit movement", "Slow idle odometry alone is not a disconnected robot", "Use unitree_pairings for identity and online/paired/connected questions", "wendy_status reports gateway connectivity, not the Go2", "A saved pairing is not a live connection", "a connected pairing is not fresh observer readiness", "Generic drift or stale telemetry does not establish a calibration fault", "Use command_outcome state/message for the exact outcome", "LatestObservationFault is historical"} {
		if !strings.Contains(prompt, guard) {
			t.Fatalf("compact prompt lost %q", guard)
		}
	}
}

func TestLocalControlSkipsReflectionButPreservesRecallAndExplicitMemory(t *testing.T) {
	profile, _ := ResolveProfile("device-control")
	opts := SessionOptions{Config: Config{Provider: "local"}, Profile: profile, Workspace: t.TempDir(), MemoryDirectory: t.TempDir(), Device: "Woof"}
	base := &engineTestExecutor{tools: []Tool{{Name: "robot_status", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	round := 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, available []Tool, _ func(string)) (Message, error) {
		round++
		if !strings.Contains(messages[0].Content, "Paw greeting") {
			t.Fatal("existing memory not recalled")
		}
		if len(available) < 4 {
			t.Fatal("explicit memory tools unavailable")
		}
		switch round {
		case 1:
			return Message{ToolCalls: []ToolCall{{ID: "read", Name: "robot_status", Arguments: json.RawMessage(`{}`)}}}, nil
		case 2:
			return Message{Content: "Observed the controller status."}, nil
		case 3:
			return Message{ToolCalls: []ToolCall{memoryTestSaveCall(t, "preference")}}, nil
		case 4:
			return Message{Content: "Remembered."}, nil
		default:
			t.Fatal("extra post-turn reflection request")
			return Message{}, nil
		}
	})
	engine, err := sessionEngine(provider, base, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.memory.store.Save(context.Background(), MemoryInput{Scope: "device", Device: "Woof", Kind: "preference", Title: "Paw greeting", Content: "The user prefers a quiet greeting", Evidence: "User said so"}); err != nil {
		t.Fatal(err)
	}
	if err := engine.Turn(context.Background(), "Paw greeting status", nil, nil); err != nil {
		t.Fatal(err)
	}
	if round != 2 {
		t.Fatalf("automatic reflection still ran: %d calls", round)
	}
	if err := engine.Turn(context.Background(), "Remember the Paw greeting preference", nil, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := engine.memory.store.Search(context.Background(), "Paw greeting", "Woof", 8)
	if err != nil || len(entries) != 1 || !strings.Contains(entries[0].Content, "Connect through cloud") {
		t.Fatalf("explicit save failed: %+v %v", entries, err)
	}
	engine.SetMemoryEnabled(false)
	for name := range controlToolNames(t, engine.executor) {
		if strings.HasPrefix(name, "memory_") {
			t.Fatal("memory opt-out ignored")
		}
	}
}

// Opt in with an already built CLI. This lists the real MCP catalog without
// invoking a device tool, so the budget includes production descriptions and
// schemas rather than small test doubles.
func TestLocalControlInitialCatalogBudget(t *testing.T) {
	executable := os.Getenv("WENDY_CHAT_CATALOG_TEST_BINARY")
	if executable == "" {
		t.Skip("set WENDY_CHAT_CATALOG_TEST_BINARY to measure the real MCP catalog")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	workspace := t.TempDir()
	base, err := NewTools(ctx, executable, workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	profile, _ := ResolveProfile("device-control")
	opts := SessionOptions{Config: Config{Provider: "local"}, Profile: profile, Workspace: workspace, MemoryDirectory: t.TempDir()}
	engine, err := sessionEngine(nil, &ProfileTools{Base: base, Profile: profile}, opts)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := engine.executor.ListTools(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wire := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		wire = append(wire, map[string]any{"type": "function", "function": map[string]any{"name": tool.Name, "description": tool.Description, "parameters": tool.Parameters}})
	}
	data, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	bytes := len(data) + len(engine.system)
	t.Logf("initial %d tools: prompt %d bytes + schemas %d bytes = %d bytes; rough four-bytes/token estimate %d (tokenizer varies)", len(tools), len(engine.system), len(data), bytes, (bytes+3)/4)
	if len(tools) != 12 {
		t.Fatalf("unexpected initial tool count: %d", len(tools))
	}
	if bytes > 16000 {
		t.Fatalf("initial payload exceeded compact 16KB budget: %d", bytes)
	}
}
