package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ModelSpec contains a user-selected connection. Agents cannot supply endpoints
// or credentials in tool arguments. Keys belong to this endpoint only.
type ModelSpec struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	BaseURL   string `json:"base_url,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`
}

func (s ModelSpec) Resolve() (Config, error) {
	c := Config{Provider: s.Provider, Model: s.Model, BaseURL: s.BaseURL, MaxTokens: s.MaxTokens, environmentApplied: true}
	if s.APIKeyEnv != "" {
		c.APIKey = os.Getenv(s.APIKeyEnv)
		if c.APIKey == "" {
			return Config{}, fmt.Errorf("model credential environment variable %s is unset", s.APIKeyEnv)
		}
	} else if key := RequiredAPIKeyEnv(c); key != "" {
		c.APIKey = os.Getenv(key)
	}
	return ResolveConfig(c)
}
func LoadAgentModels(file string) (map[string]Config, error) {
	configs := map[string]Config{}
	if file == "" {
		return configs, nil
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var specs map[string]ModelSpec
	dec := json.NewDecoder(io.LimitReader(f, 65537))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&specs); err != nil {
		return nil, fmt.Errorf("agent models: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("agent models must contain one JSON object")
	}
	for name, spec := range specs {
		profile, err := ResolveProfile(name)
		if err != nil {
			return nil, err
		}
		if name != profile.Name {
			return nil, fmt.Errorf("agent model key %q must use canonical profile name %q", name, profile.Name)
		}
		c, err := spec.Resolve()
		if err != nil {
			return nil, fmt.Errorf("agent model %s: %w", name, err)
		}
		configs[name] = c
	}
	return configs, nil
}

// Session owns tools and the engine for either a terminal or a service task.
// Closing a session terminates the MCP connection and managed media workers.
type Session struct {
	Engine *Engine
	tools  *Tools
}
type SessionOptions struct {
	SystemInstructions                             string
	DelegationDepth                                int
	Config                                         Config
	Profile                                        Profile
	Executable, Workspace, Device, MemoryDirectory string
	NoMemory                                       bool
	AgentModels                                    map[string]Config
	// ApprovedTools is an optional execution allowlist used by unattended services.
	// nil preserves the interactive tool approval flow.
	Peers         map[string]PeerSpec
	ApprovedTools []string
}

func NewSession(ctx context.Context, opts SessionOptions) (*Session, error) {
	profile, err := ResolveProfile(opts.Profile.Name)
	if err != nil {
		return nil, err
	}
	opts.Profile = profile
	if err := ValidatePeers(opts.Peers); err != nil {
		return nil, err
	}
	provider, err := NewProvider(opts.Config)
	if err != nil {
		return nil, err
	}
	tools, err := NewTools(ctx, opts.Executable, opts.Workspace, opts.Device)
	if err != nil {
		return nil, err
	}
	supervisor := &agentSupervisor{options: opts, gate: make(chan struct{}, 1)}
	base := &ProfileTools{Base: tools, Profile: opts.Profile}
	delegated := &agentExecutor{base: base, supervisor: supervisor}
	engine, err := sessionEngine(provider, delegated, opts)
	if err != nil {
		_ = tools.Close()
		return nil, err
	}
	return &Session{Engine: engine, tools: tools}, nil
}
func sessionEngine(provider Provider, executor Executor, opts SessionOptions) (*Engine, error) {
	if opts.ApprovedTools != nil {
		executor = &serviceTools{base: executor, allowed: opts.ApprovedTools}
	}
	store, err := NewProfileMemoryStore(opts.MemoryDirectory, opts.Workspace, opts.Device, opts.Profile)
	if err != nil {
		return nil, err
	}
	memory := NewMemoryTools(executor, store)
	prompt := opts.Profile.Prompt(opts.Workspace, opts.Device)
	if opts.SystemInstructions != "" {
		prompt += "\n\n" + opts.SystemInstructions
	}
	if opts.ApprovedTools != nil {
		prompt += "\nThis is an unattended service task. Only the configured mutation tools are authorized; unavailable tools are blocked. There is no terminal user to answer approval prompts. Report missing capabilities explicitly."
	}
	engine := NewEngine(provider, memory, prompt)
	engine.SetMemoryEnabled(!opts.NoMemory)
	return engine, nil
}
func (s *Session) Close() error { return s.tools.Close() }

// Service policies apply before tool discovery AND execution. Only listed
// mutation tools can run unattended; reads and local memory retain defaults.
type serviceTools struct {
	base    Executor
	allowed []string
}

func (s *serviceTools) permits(t Tool) bool {
	if !t.RequiresApproval {
		return true
	}
	for _, name := range s.allowed {
		if name == t.Name {
			return true
		}
	}
	return false
}
func (s *serviceTools) ListTools(ctx context.Context) ([]Tool, error) {
	list, err := s.base.ListTools(ctx)
	if err != nil {
		return nil, err
	}
	out := []Tool{}
	for _, t := range list {
		if s.permits(t) {
			out = append(out, t)
		}
	}
	return out, nil
}
func (s *serviceTools) Execute(ctx context.Context, c ToolCall) (string, error) {
	r, e := s.ExecuteResult(ctx, c)
	return r.Text, e
}
func (s *serviceTools) ExecuteResult(ctx context.Context, c ToolCall) (ToolResult, error) {
	list, err := s.ListTools(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	for _, t := range list {
		if t.Name == c.Name {
			if m, ok := s.base.(MediaExecutor); ok {
				return m.ExecuteResult(ctx, c)
			}
			v, e := s.base.Execute(ctx, c)
			return ToolResult{Text: v}, e
		}
	}
	return ToolResult{}, fmt.Errorf("service policy denies %s", c.Name)
}

type turnRuntimeKey struct{}
type turnRuntime struct {
	approve       ApproveFunc
	emit          func(Event)
	memoryEnabled func() bool
	count         atomic.Int32
}

var delegateTool = Tool{Name: "agent_delegate", Description: "Run 1-4 independent specialist tasks concurrently and wait for their structured results. Each has a private conversation and isolated device connection. Children use configured profile models, inherit tool restrictions and approvals, and cannot delegate. Include required context and explicit device identity; assign disjoint edits and device actions.", Parameters: json.RawMessage(`{"type":"object","properties":{"tasks":{"type":"array","minItems":1,"maxItems":4,"items":{"type":"object","properties":{"profile":{"type":"string","enum":["general","developer","simulation","debugger","fleet","device-reasoning","device-sensors","device-control"]},"prompt":{"type":"string","minLength":1,"maxLength":16000},"device":{"type":"string","maxLength":256}},"required":["profile","prompt"],"additionalProperties":false}}},"required":["tasks"],"additionalProperties":false}`)}

type agentTask struct {
	Profile string `json:"profile"`
	Prompt  string `json:"prompt"`
	Device  string `json:"device,omitempty"`
}
type agentResult struct {
	ID       string `json:"id"`
	Profile  string `json:"profile"`
	Device   string `json:"device,omitempty"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	State    string `json:"state"`
	Text     string `json:"text,omitempty"`
	Error    string `json:"error,omitempty"`
}
type agentSupervisor struct {
	options SessionOptions
	gate    chan struct{}
	// factory is injected in tests. Each call must return a fresh engine/executor.
	factory func(context.Context, SessionOptions, *Profile) (*Engine, func(), error)
}
type agentExecutor struct {
	base       Executor
	supervisor *agentSupervisor
}

func (a *agentExecutor) ListTools(ctx context.Context) ([]Tool, error) {
	list, e := a.base.ListTools(ctx)
	if e != nil {
		return nil, e
	}
	if len(a.supervisor.options.Peers) > 0 {
		list = append(list, a.supervisor.peerTool())
	}
	return append(list, delegateTool), nil
}
func (a *agentExecutor) Execute(ctx context.Context, c ToolCall) (string, error) {
	r, e := a.ExecuteResult(ctx, c)
	return r.Text, e
}
func (a *agentExecutor) ExecuteResult(ctx context.Context, c ToolCall) (ToolResult, error) {
	if c.Name == remoteTool.Name {
		v, e := a.supervisor.remote(ctx, c)
		return ToolResult{Text: v}, e
	}
	if c.Name == delegateTool.Name {
		v, e := a.supervisor.delegate(ctx, c)
		return ToolResult{Text: v}, e
	}
	if m, ok := a.base.(MediaExecutor); ok {
		return m.ExecuteResult(ctx, c)
	}
	v, e := a.base.Execute(ctx, c)
	return ToolResult{Text: v}, e
}
func (s *agentSupervisor) child(ctx context.Context, opts SessionOptions, parent *Profile) (*Engine, func(), error) {
	if s.factory != nil {
		return s.factory(ctx, opts, parent)
	}
	provider, err := NewProvider(opts.Config)
	if err != nil {
		return nil, nil, err
	}
	tools, err := NewTools(ctx, opts.Executable, opts.Workspace, opts.Device)
	if err != nil {
		return nil, nil, err
	}
	// Serialize mutating calls across children. Continuous motion ownership remains
	// the local controller's responsibility, not the duration of a tool request.
	executor := &serializedTools{base: &ProfileTools{Base: tools, Profile: opts.Profile, Parent: parent}, gate: s.gate}
	engine, err := sessionEngine(provider, executor, opts)
	if err != nil {
		_ = tools.Close()
		return nil, nil, err
	}
	return engine, func() { _ = tools.Close() }, nil
}
func (s *agentSupervisor) delegate(ctx context.Context, call ToolCall) (string, error) {
	if err := validateArguments(delegateTool, call.Arguments); err != nil {
		return "", err
	}
	runtime, ok := ctx.Value(turnRuntimeKey{}).(*turnRuntime)
	if !ok {
		return "", errors.New("delegation requires an active turn")
	}
	var args struct {
		Tasks []agentTask `json:"tasks"`
	}
	_ = json.Unmarshal(call.Arguments, &args)
	count := runtime.count.Add(int32(len(args.Tasks)))
	if count > 12 {
		return "", errors.New("this turn reached its 12-child limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	results := make([]agentResult, len(args.Tasks))
	var wg sync.WaitGroup
	// The UI has one approval prompt; serialize requests without holding the tool
	// execution gate, so a child waiting for permission cannot deadlock another.
	approvalGate := make(chan struct{}, 1)
	var eventMu sync.Mutex
	emit := func(e Event) { eventMu.Lock(); defer eventMu.Unlock(); runtime.emit(e) }
	for i, task := range args.Tasks {
		wg.Add(1)
		go func(i int, task agentTask) {
			defer wg.Done()
			opts := s.options
			opts.Profile, _ = ResolveProfile(task.Profile)
			opts.Device = firstValue(task.Device, opts.Device)
			if runtime.memoryEnabled != nil {
				opts.NoMemory = !runtime.memoryEnabled()
			}
			if config, ok := opts.AgentModels[task.Profile]; ok {
				opts.Config = config
			}
			id := fmt.Sprintf("agent-%d", int(count)-len(args.Tasks)+i+1)
			r := agentResult{ID: id, Profile: task.Profile, Device: opts.Device, Provider: opts.Config.Provider, Model: opts.Config.Model, State: "failed"}
			defer func() {
				results[i] = r
				emit(Event{Type: "agent_done", AgentID: id, Profile: task.Profile, Text: r.State + ": " + firstValue(r.Error, r.Text)})
			}()
			engine, close, err := s.child(ctx, opts, &s.options.Profile)
			if err != nil {
				r.Error = err.Error()
				return
			}
			defer close()
			if engine.memory != nil {
				engine.memory.parentEnabled = runtime.memoryEnabled
			}
			childEmit := func(e Event) {
				e.AgentID = id
				e.Profile = task.Profile
				if e.Call != nil {
					copy := *e.Call
					copy.AgentID = id
					copy.Profile = task.Profile
					e.Call = &copy
				}
				emit(e)
			}
			approve := func(ctx context.Context, c ToolCall) (bool, error) {
				if runtime.approve == nil {
					return false, nil
				}
				select {
				case approvalGate <- struct{}{}:
				case <-ctx.Done():
					return false, ctx.Err()
				}
				defer func() { <-approvalGate }()
				c.AgentID = id
				c.Profile = task.Profile
				return runtime.approve(ctx, c)
			}
			emit(Event{Type: "agent_start", AgentID: id, Profile: task.Profile, Text: task.Prompt})
			err = engine.Turn(ctx, task.Prompt, childEmit, approve)
			for _, m := range engine.Messages() {
				if m.Role == "assistant" && len(m.ToolCalls) == 0 {
					r.Text = m.Content
				}
			}
			r.Text = boundedText(r.Text, 6000)
			if err != nil {
				r.Error = err.Error()
				if errors.Is(err, context.Canceled) {
					r.State = "canceled"
				}
				return
			}
			r.State = "completed"
		}(i, task)
	}
	wg.Wait()
	data, err := json.Marshal(results)
	return string(data), err
}
func boundedText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "\n[Truncated.]"
}

type serializedTools struct {
	base Executor
	gate chan struct{}
}

func (s *serializedTools) ListTools(ctx context.Context) ([]Tool, error) {
	return s.base.ListTools(ctx)
}
func (s *serializedTools) Execute(ctx context.Context, c ToolCall) (string, error) {
	r, e := s.ExecuteResult(ctx, c)
	return r.Text, e
}
func (s *serializedTools) ExecuteResult(ctx context.Context, c ToolCall) (ToolResult, error) {
	list, err := s.base.ListTools(ctx)
	if err != nil {
		return ToolResult{}, err
	}
	for _, t := range list {
		if t.Name == c.Name && t.RequiresApproval {
			select {
			case s.gate <- struct{}{}:
			case <-ctx.Done():
				return ToolResult{}, ctx.Err()
			}
			defer func() { <-s.gate }()
			break
		}
	}
	if m, ok := s.base.(MediaExecutor); ok {
		return m.ExecuteResult(ctx, c)
	}
	v, e := s.base.Execute(ctx, c)
	return ToolResult{Text: v}, e
}
