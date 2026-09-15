package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
)

var memoryTools = []Tool{
	{
		Name: "memory_search", Description: "Search persistent lessons, verified procedures, facts, and user preferences. Search before repeating a task; use device to recall notes for a named device. Historical notes can be stale: verify current state and permissions.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","maxLength":1000},"device":{"type":"string","maxLength":256},"limit":{"type":"integer","minimum":1,"maximum":50}},"additionalProperties":false}`),
	},
	{
		Name: "memory_save", Description: "Remember a concise reusable lesson or verified procedure, or a fact/preference explicitly supplied by the user. Use the same scope and title to correct an existing note. Procedures and lessons must cite source_call_ids from tools executed in this turn; procedures require a successful result. Include prerequisites, exact working commands, failure cause, and how success was checked when known. Never save credentials, raw transcripts, guesses, temporary device state, or permission to act in the future. Device scope requires the actual device name. Saves local memory automatically.",
		Parameters: json.RawMessage(`{"type":"object","properties":{"scope":{"type":"string","enum":["workspace","device","global"]},"device":{"type":"string","maxLength":256},"kind":{"type":"string","enum":["procedure","lesson","fact","preference"]},"title":{"type":"string","minLength":1,"maxLength":160},"content":{"type":"string","minLength":1,"maxLength":6000},"evidence":{"type":"string","minLength":1,"maxLength":2000},"source_call_ids":{"type":"array","items":{"type":"string"},"maxItems":12}},"required":["scope","kind","title","content","evidence"],"additionalProperties":false}`),
	},
	{
		Name: "memory_forget", Description: "Delete one persistent memory by its ID, after finding it with memory_search. Requires approval. Use this to remove an incorrect or unwanted note.", RequiresApproval: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","pattern":"^[a-f0-9]{64}$"}},"required":["id"],"additionalProperties":false}`),
	},
}

type memoryObservation struct {
	ID        string `json:"call_id"`
	Tool      string `json:"tool"`
	Arguments string `json:"arguments"`
	Output    string `json:"output"`
	Succeeded bool   `json:"succeeded"`
}

// MemoryTools adds local, provider-independent memory without changing the MCP
// server or the device's storage. All normal execution still uses the base tools.
type MemoryTools struct {
	base           Executor
	store          *MemoryStore
	enabled        atomic.Bool
	mu             sync.Mutex
	observed       []memoryObservation
	saved          bool
	forgotten      map[string]bool
	revision       uint64
	learningCancel context.CancelFunc
}

func NewMemoryTools(base Executor, store *MemoryStore) *MemoryTools {
	m := &MemoryTools{base: base, store: store}
	m.enabled.Store(store != nil)
	return m
}

func (m *MemoryTools) ListTools(ctx context.Context) ([]Tool, error) {
	list, err := m.base.ListTools(ctx)
	if err != nil || !m.enabled.Load() {
		return list, err
	}
	return append(append([]Tool(nil), list...), memoryTools...), nil
}

func (m *MemoryTools) Execute(ctx context.Context, call ToolCall) (string, error) {
	result, err := m.ExecuteResult(ctx, call)
	return result.Text, err
}

func (m *MemoryTools) ExecuteResult(ctx context.Context, call ToolCall) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	for _, tool := range memoryTools {
		if call.Name != tool.Name {
			continue
		}
		if !m.enabled.Load() {
			return ToolResult{}, errors.New("memory is off; use /memory on to enable it")
		}
		if err := validateArguments(tool, call.Arguments); err != nil {
			return ToolResult{}, err
		}
		text, err := m.executeMemory(ctx, call)
		return ToolResult{Text: text}, err
	}
	var result ToolResult
	var err error
	if executor, ok := m.base.(MediaExecutor); ok {
		result, err = executor.ExecuteResult(ctx, call)
	} else {
		result.Text, err = m.base.Execute(ctx, call)
	}
	return result, err
}

// Observe only executed results after the engine has validated media. Denials,
// invalid calls, and rejected images must not become successful evidence.
func (m *MemoryTools) observe(call ToolCall, result ToolResult, err error) {
	if strings.HasPrefix(call.Name, "memory_") {
		return
	}
	if m.enabled.Load() {
		output := result.Text
		if err != nil {
			output = err.Error() + "\n" + output
		}
		m.mu.Lock()
		m.observed = append(m.observed, memoryObservation{
			ID: call.ID, Tool: call.Name, Arguments: memoryExcerpt(string(call.Arguments), 2048),
			Output: memoryExcerpt(output, 3072), Succeeded: err == nil,
		})
		if len(m.observed) > 24 {
			m.observed = append([]memoryObservation(nil), m.observed[len(m.observed)-24:]...)
		}
		m.mu.Unlock()
	}
}

func (m *MemoryTools) executeMemory(ctx context.Context, call ToolCall) (string, error) {
	switch call.Name {
	case "memory_search":
		var args struct {
			Query, Device string
			Limit         int
		}
		_ = json.Unmarshal(call.Arguments, &args)
		entries, err := m.store.Search(ctx, args.Query, args.Device, args.Limit)
		if err != nil {
			return "", err
		}
		return memoryJSON(entries), nil
	case "memory_save":
		var args struct {
			MemoryInput
			SourceCallIDs []string `json:"source_call_ids"`
		}
		_ = json.Unmarshal(call.Arguments, &args)
		m.mu.Lock()
		defer m.mu.Unlock()
		byID := make(map[string]memoryObservation, len(m.observed))
		for _, observation := range m.observed {
			byID[observation.ID] = observation
		}
		success := false
		for _, id := range args.SourceCallIDs {
			observation, exists := byID[id]
			if !exists {
				return "", fmt.Errorf("memory evidence %q is not a tool executed in this turn", id)
			}
			success = success || observation.Succeeded
		}
		if (args.Kind == "procedure" || args.Kind == "lesson") && len(args.SourceCallIDs) == 0 {
			return "", errors.New("procedures and lessons require source_call_ids from this turn's tool results")
		}
		if args.Kind == "procedure" && !success {
			return "", errors.New("a procedure needs successful tool evidence; save a failed attempt as a lesson instead")
		}
		if memoryContainsSecret(args.Title + "\n" + args.Content + "\n" + args.Evidence) {
			return "", errors.New("memory appears to contain a credential; omit secrets and use a placeholder")
		}
		if !m.enabled.Load() {
			return "", errors.New("memory is off")
		}
		identity := MemoryEntry{Scope: args.Scope, Device: args.Device, Title: args.Title}
		if identity.Scope == "workspace" {
			identity.Workspace = m.store.workspace
		}
		if m.forgotten[memoryID(identity)] {
			return "", errors.New("this note was forgotten during the current turn; do not recreate it")
		}
		entry, err := m.store.Save(ctx, args.MemoryInput)
		if err != nil {
			return "", err
		}
		m.saved = true
		return "Remembered " + entry.Title + " (" + entry.ID + ").", nil
	case "memory_forget":
		var args struct{ ID string }
		_ = json.Unmarshal(call.Arguments, &args)
		if err := m.forget(ctx, args.ID); err != nil {
			return "", err
		}
		return "Forgot memory " + args.ID + ".", nil
	}
	return "", errors.New("unknown memory tool")
}

func (m *MemoryTools) beginTurn() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observed, m.saved = nil, false
	m.forgotten = make(map[string]bool)
}

func (m *MemoryTools) forget(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.store.Delete(ctx, id); err != nil {
		return err
	}
	if m.forgotten == nil {
		m.forgotten = make(map[string]bool)
	}
	m.forgotten[id] = true
	m.saved = true // A memory management request must not trigger relearning.
	m.revision++
	if m.learningCancel != nil {
		m.learningCancel()
	}
	return nil
}

func memoryExcerpt(value string, limit int) string {
	value = chatSanitize(value)
	if len(value) > limit {
		value = strings.ToValidUTF8(value[:limit], "") + " [truncated]"
	}
	return value
}

func memoryJSON(entries []MemoryEntry) string {
	if len(entries) == 0 {
		return "No matching memories."
	}
	// Bound injected context and search output without truncating JSON records.
	total := len(entries)
	for len(entries) > 0 {
		data, _ := json.MarshalIndent(entries, "", "  ")
		if len(data) <= 24*1024 {
			if len(entries) < total {
				return fmt.Sprintf("%s\n[Showing %d of %d matched notes within the output limit; use memory_search with a more specific query.]", data, len(entries), total)
			}
			return string(data)
		}
		entries = entries[:len(entries)-1]
	}
	return "No matching memories."
}

// This catches common credential formats as a backstop, not a general secret
// detector. The model is separately instructed to store distilled notes only.
func memoryContainsSecret(value string) bool {
	for _, token := range strings.FieldsFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) {
		if looksLikeStandaloneCredential(token) {
			return true
		}
	}
	return strings.Contains(value, "-----BEGIN PRIVATE KEY-----") || strings.Contains(value, "-----BEGIN RSA PRIVATE KEY-----")
}

func (e *Engine) MemoryEnabled() bool {
	return e.memory != nil && e.memory.enabled.Load()
}

func (e *Engine) SetMemoryEnabled(enabled bool) {
	if e.memory != nil && e.memory.store != nil {
		e.memory.mu.Lock()
		defer e.memory.mu.Unlock()
		if e.memory.enabled.Load() == enabled {
			return
		}
		e.memory.enabled.Store(enabled)
		e.memory.revision++
		if !enabled && e.memory.learningCancel != nil {
			e.memory.learningCancel()
		}
	}
}

func (e *Engine) MemoryNotes(ctx context.Context, query string) ([]MemoryEntry, error) {
	if e.memory == nil || e.memory.store == nil {
		return nil, errors.New("memory is unavailable for this session")
	}
	return e.memory.store.Search(ctx, query, "", 50)
}

func (e *Engine) MemoryNote(ctx context.Context, ref string) (MemoryEntry, error) {
	if e.memory == nil || e.memory.store == nil {
		return MemoryEntry{}, errors.New("memory is unavailable for this session")
	}
	return e.memory.store.Lookup(ctx, ref)
}

func (e *Engine) ForgetMemory(ctx context.Context, id string) error {
	if e.memory == nil || e.memory.store == nil {
		return errors.New("memory is unavailable for this session")
	}
	if len(id) != 64 {
		entry, err := e.memory.store.Lookup(ctx, id)
		if err != nil {
			return err
		}
		id = entry.ID
	}
	return e.memory.forget(ctx, id)
}
