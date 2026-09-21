package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDelegationRunsInParallelWithIsolatedContextsAndApprovals(t *testing.T) {
	profile, _ := ResolveProfile("general")
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	var approved, executed atomic.Int32
	var enginesMu sync.Mutex
	var engines []*Engine
	supervisor := &agentSupervisor{options: SessionOptions{Profile: profile, Config: Config{Provider: "local", Model: "parent"}, Device: "parent-device", AgentModels: map[string]Config{"debugger": {Provider: "ollama", Model: "small"}}}, gate: make(chan struct{}, 1)}
	supervisor.factory = func(ctx context.Context, opts SessionOptions, parent *Profile) (*Engine, func(), error) {
		if opts.Profile.Name == "debugger" && (opts.Config.Model != "small" || opts.Device != "child-device") {
			t.Error("lost child config")
		}
		round := 0
		provider := engineTestProvider(func(ctx context.Context, m []Message, tools []Tool, _ func(string)) (Message, error) {
			round++
			if round == 1 {
				if len(m) != 2 || strings.Contains(m[1].Content, "parent-secret-context") {
					t.Error("child inherited transcript")
				}
				started <- struct{}{}
				select {
				case <-release:
				case <-ctx.Done():
					return Message{}, ctx.Err()
				}
				return Message{ToolCalls: []ToolCall{{ID: "write", Name: "write", Arguments: json.RawMessage(`{}`)}}}, nil
			}
			return Message{Content: "child done"}, nil
		})
		executor := &engineTestExecutor{tools: []Tool{{Name: "write", RequiresApproval: true}}, run: func(context.Context, ToolCall) (string, error) { executed.Add(1); return "ok", nil }}
		engine := NewEngine(provider, executor, opts.Profile.Name)
		enginesMu.Lock()
		engines = append(engines, engine)
		enginesMu.Unlock()
		return engine, func() {}, nil
	}
	runtime := &turnRuntime{approve: func(ctx context.Context, c ToolCall) (bool, error) {
		if c.AgentID == "" || c.Profile == "" {
			t.Error("unattributed approval")
		}
		approved.Add(1)
		return false, nil
	}, emit: func(Event) {}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, turnRuntimeKey{}, runtime)
	result := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		out, err := supervisor.delegate(ctx, ToolCall{Arguments: json.RawMessage(`{"tasks":[{"profile":"developer","prompt":"build"},{"profile":"debugger","prompt":"debug","device":"child-device"}]}`)})
		result <- out
		errs <- err
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("children did not start concurrently")
		}
	}
	close(release)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	var results []agentResult
	if err := json.Unmarshal([]byte(<-result), &results); err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[0].State != "completed" || results[1].Text != "child done" || approved.Load() != 2 || executed.Load() != 0 {
		t.Fatalf("results=%+v approvals=%d executions=%d", results, approved.Load(), executed.Load())
	}
	if engines[0] == engines[1] {
		t.Fatal("children share an engine")
	}
}
func TestDelegationCancellationJoinsChildren(t *testing.T) {
	profile, _ := ResolveProfile("general")
	started := make(chan struct{})
	closed := make(chan struct{})
	supervisor := &agentSupervisor{options: SessionOptions{Profile: profile}, factory: func(context.Context, SessionOptions, *Profile) (*Engine, func(), error) {
		p := engineTestProvider(func(ctx context.Context, _ []Message, _ []Tool, _ func(string)) (Message, error) {
			close(started)
			<-ctx.Done()
			return Message{}, ctx.Err()
		})
		return NewEngine(p, &engineTestExecutor{}, "child"), func() { close(closed) }, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	ctx = context.WithValue(ctx, turnRuntimeKey{}, &turnRuntime{emit: func(Event) {}})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = supervisor.delegate(ctx, ToolCall{Arguments: json.RawMessage(`{"tasks":[{"profile":"debugger","prompt":"wait"}]}`)})
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("child did not stop")
	}
	select {
	case <-closed:
	default:
		t.Fatal("child resources were not closed")
	}
}
func TestDelegationBudgetAndFailures(t *testing.T) {
	var calls int
	supervisor := &agentSupervisor{factory: func(context.Context, SessionOptions, *Profile) (*Engine, func(), error) {
		calls++
		return nil, nil, errors.New("unavailable")
	}}
	runtime := &turnRuntime{emit: func(Event) {}}
	runtime.count.Store(12)
	ctx := context.WithValue(context.Background(), turnRuntimeKey{}, runtime)
	call := ToolCall{Arguments: json.RawMessage(`{"tasks":[{"profile":"debugger","prompt":"debug"}]}`)}
	if _, err := supervisor.delegate(ctx, call); err == nil || calls != 0 {
		t.Fatal("budget not enforced")
	}
	runtime.count.Store(0)
	out, err := supervisor.delegate(ctx, call)
	if err != nil || !strings.Contains(out, "unavailable") {
		t.Fatal(out, err)
	}
}
func TestChildMemoryFollowsParentToggle(t *testing.T) {
	var enabled atomic.Bool
	enabled.Store(true)
	memory := NewMemoryTools(&engineTestExecutor{}, nil)
	memory.enabled.Store(true)
	memory.parentEnabled = enabled.Load
	engine := NewEngine(nil, memory, "")
	if !engine.MemoryEnabled() {
		t.Fatal("memory unexpectedly off")
	}
	enabled.Store(false)
	if engine.MemoryEnabled() {
		t.Fatal("child memory remained on")
	}
	if _, err := memory.Execute(context.Background(), ToolCall{Name: "memory_save", Arguments: json.RawMessage(`{}`)}); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatal("child can save after parent paused", err)
	}
}
