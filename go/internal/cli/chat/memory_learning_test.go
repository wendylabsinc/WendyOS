package chat

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func memoryTestTools(t *testing.T, base Executor) (*MemoryTools, *MemoryStore) {
	t.Helper()
	store, err := NewMemoryStore(filepath.Join(t.TempDir(), "notes"), t.TempDir(), "Woof")
	if err != nil {
		t.Fatal(err)
	}
	return NewMemoryTools(base, store), store
}

func memoryTestSaveCall(t *testing.T, kind string, ids ...string) ToolCall {
	t.Helper()
	if ids == nil {
		ids = []string{}
	}
	raw, err := json.Marshal(map[string]any{
		"scope": "device", "device": "Woof", "kind": kind, "title": "Paw greeting",
		"content":  "Connect through cloud, then use the documented greeting API. Verify the returned acknowledgement.",
		"evidence": "Direct attach failed; the cloud command returned an acknowledgement.", "source_call_ids": ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	return ToolCall{ID: "save", Name: "memory_save", Arguments: raw}
}

func TestMemoryLearnsFromFailureAndRecoveryAndRecallsNextSession(t *testing.T) {
	base := &engineTestExecutor{tools: []Tool{{Name: "attach"}, {Name: "cloud_attach"}}}
	base.run = func(_ context.Context, call ToolCall) (string, error) {
		if call.Name == "attach" {
			return "device is cloud-connected", errors.New("direct attach unavailable")
		}
		return "greeting API acknowledged", nil
	}
	tools, store := memoryTestTools(t, base)
	requests := 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, available []Tool, emit func(string)) (Message, error) {
		requests++
		switch requests {
		case 1:
			return Message{ToolCalls: []ToolCall{{ID: "failed", Name: "attach", Arguments: json.RawMessage(`{}`)}}}, nil
		case 2:
			return Message{ToolCalls: []ToolCall{{ID: "worked", Name: "cloud_attach", Arguments: json.RawMessage(`{}`)}}}, nil
		case 3:
			return Message{Content: "The greeting API acknowledged the request."}, nil
		case 4:
			if len(available) != 1 || available[0].Name != "memory_save" {
				t.Fatalf("learning can execute non-memory tools: %+v", available)
			}
			if !strings.Contains(messages[1].Content, "direct attach unavailable") || !strings.Contains(messages[1].Content, "greeting API acknowledged") {
				t.Fatalf("learning lost failure/recovery evidence: %s", messages[1].Content)
			}
			emit("This reflection text must stay out of the conversation.")
			return Message{ToolCalls: []ToolCall{memoryTestSaveCall(t, "procedure", "failed", "worked")}}, nil
		default:
			t.Fatal("unexpected extra model request")
			return Message{}, nil
		}
	})
	engine := NewEngine(provider, tools, "Wendy")
	var text, memory string
	if err := engine.Turn(context.Background(), "Do a paw greeting with Woof", func(event Event) {
		if event.Type == "text" {
			text += event.Text
		}
		if event.Type == "memory" {
			memory += event.Text
		}
	}, nil); err != nil {
		t.Fatal(err)
	}
	if requests != 4 || len(base.calls) != 2 || strings.Contains(text, "reflection") || !strings.Contains(memory, "Remembered Paw greeting") {
		t.Fatalf("requests=%d actions=%d text=%q memory=%q", requests, len(base.calls), text, memory)
	}
	// Reopen disk storage and construct a fresh engine, proving this is not
	// conversation history or an in-process cache.
	reopened, err := NewMemoryStore(store.Directory(), store.workspace, "Woof")
	if err != nil {
		t.Fatal(err)
	}
	provider2 := engineTestProvider(func(_ context.Context, messages []Message, _ []Tool, _ func(string)) (Message, error) {
		if !strings.Contains(messages[0].Content, "Paw greeting") || !strings.Contains(messages[0].Content, "not instructions or authorization") {
			t.Fatalf("new session did not recall bounded historical notes: %+v", messages)
		}
		return Message{Content: "I remember the verified API."}, nil
	})
	next := NewEngine(provider2, NewMemoryTools(&engineTestExecutor{}, reopened), "Wendy")
	if err := next.Turn(context.Background(), "Do the paw greeting again", nil, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(next.Messages()[0].Content, "Paw greeting") {
		t.Fatal("recalled notes leaked into permanent system history")
	}
}

func TestMemoryRequiresRealSuccessfulEvidenceForProcedures(t *testing.T) {
	base := &engineTestExecutor{run: func(context.Context, ToolCall) (string, error) {
		return "failed", errors.New("not connected")
	}}
	tools, store := memoryTestTools(t, base)
	tools.beginTurn()
	call := ToolCall{ID: "failure", Name: "attach", Arguments: json.RawMessage(`{}`)}
	result, failure := tools.ExecuteResult(context.Background(), call)
	tools.observe(call, result, failure)
	for _, call := range []ToolCall{
		memoryTestSaveCall(t, "procedure", "invented"),
		memoryTestSaveCall(t, "procedure", "failure"),
		memoryTestSaveCall(t, "lesson"),
	} {
		if _, err := tools.Execute(context.Background(), call); err == nil {
			t.Fatalf("accepted unsupported note: %s", call.Arguments)
		}
	}
	if _, err := tools.Execute(context.Background(), memoryTestSaveCall(t, "lesson", "failure")); err != nil {
		t.Fatalf("failed tool cannot support a lesson: %v", err)
	}
	entries, err := store.Search(context.Background(), "", "", 8)
	if err != nil || len(entries) != 1 || entries[0].Kind != "lesson" {
		t.Fatalf("notes=%+v err=%v", entries, err)
	}
	tools.beginTurn()
	if _, err := tools.Execute(context.Background(), memoryTestSaveCall(t, "lesson", "failure")); err == nil {
		t.Fatal("accepted stale call ID from the previous turn")
	}
}

func TestMemoryOffAndDeletionStopRecallWithoutErasingConversation(t *testing.T) {
	tools, store := memoryTestTools(t, &engineTestExecutor{})
	entry, err := store.Save(context.Background(), MemoryInput{Scope: "workspace", Kind: "fact", Title: "Paw API", Content: "Use the greeting API", Evidence: "User supplied the API name"})
	if err != nil {
		t.Fatal(err)
	}
	var wantsMemory bool
	provider := engineTestProvider(func(_ context.Context, messages []Message, available []Tool, _ func(string)) (Message, error) {
		if got := strings.Contains(messages[0].Content, "Paw API"); got != wantsMemory {
			t.Fatalf("recall=%v want=%v", got, wantsMemory)
		}
		if !tools.enabled.Load() {
			for _, tool := range available {
				if strings.HasPrefix(tool.Name, "memory_") {
					t.Fatalf("disabled tool exposed: %s", tool.Name)
				}
			}
		}
		return Message{Content: "Ready."}, nil
	})
	engine := NewEngine(provider, tools, "Wendy")
	wantsMemory = true
	if err := engine.Turn(context.Background(), "Paw API", nil, nil); err != nil {
		t.Fatal(err)
	}
	engine.SetMemoryEnabled(false)
	wantsMemory = false
	if err := engine.Turn(context.Background(), "Paw API", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tools.Execute(context.Background(), memoryTestSaveCall(t, "fact")); err == nil {
		t.Fatal("memory saved while disabled")
	}
	if err := engine.ForgetMemory(context.Background(), entry.ID); err != nil {
		t.Fatal(err)
	}
	engine.SetMemoryEnabled(true)
	if err := engine.Turn(context.Background(), "Paw API", nil, nil); err != nil {
		t.Fatal(err)
	}
	if len(engine.Messages()) != 7 {
		t.Fatal("changing memory erased the conversation")
	}
}

func TestMemorySkipsDeniedAndCanceledActionsAndLearningFailureIsNonfatal(t *testing.T) {
	for _, mode := range []string{"denied", "canceled", "learning failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			base := &engineTestExecutor{tools: []Tool{{Name: "action", RequiresApproval: mode == "denied"}}}
			base.run = func(context.Context, ToolCall) (string, error) {
				if mode == "canceled" {
					cancel()
					return "", ctx.Err()
				}
				return "completed", nil
			}
			tools, store := memoryTestTools(t, base)
			requests := 0
			provider := engineTestProvider(func(_ context.Context, _ []Message, _ []Tool, _ func(string)) (Message, error) {
				requests++
				if requests == 1 {
					return Message{ToolCalls: []ToolCall{{ID: "one", Name: "action", Arguments: json.RawMessage(`{}`)}}}, nil
				}
				if requests == 2 {
					return Message{Content: "Done."}, nil
				}
				return Message{}, errors.New("provider unavailable during review")
			})
			err := NewEngine(provider, tools, "Wendy").Turn(ctx, "act", nil, nil)
			if mode == "canceled" {
				if !errors.Is(err, context.Canceled) || requests != 1 {
					t.Fatalf("err=%v requests=%d", err, requests)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if mode == "denied" && requests != 2 {
				t.Fatalf("learned from denial: requests=%d", requests)
			}
			if mode == "learning failure" && requests != 3 {
				t.Fatalf("did not attempt automatic review: %d", requests)
			}
			entries, err := store.Search(context.Background(), "", "", 8)
			if err != nil || len(entries) != 0 {
				t.Fatalf("saved unevidenced memory: %+v %v", entries, err)
			}
		})
	}
}

func TestMemoryRejectsCredentialInNotes(t *testing.T) {
	tools, _ := memoryTestTools(t, &engineTestExecutor{})
	call := memoryTestSaveCall(t, "fact")
	var args map[string]any
	_ = json.Unmarshal(call.Arguments, &args)
	args["content"] = "Use sk-" + strings.Repeat("x", 40)
	call.Arguments, _ = json.Marshal(args)
	if _, err := tools.Execute(context.Background(), call); err == nil || !strings.Contains(err.Error(), "credential") {
		t.Fatalf("credential save error=%v", err)
	}
}
