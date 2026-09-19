package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMemoryRejectedImageCannotProveProcedure(t *testing.T) {
	base, err := newWorkspaceTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	base.mcp = &snapshotMCP{image: Image{MIMEType: "image/png", Data: "%%%"}}
	tools, store := memoryTestTools(t, base)
	requests := 0
	provider := engineTestProvider(func(_ context.Context, messages []Message, available []Tool, _ func(string)) (Message, error) {
		requests++
		switch requests {
		case 1:
			return Message{ToolCalls: []ToolCall{{ID: "snapshot", Name: "camera_snapshot", Arguments: json.RawMessage(`{}`)}}}, nil
		case 2:
			result := messages[len(messages)-1]
			if result.ToolCallID != "snapshot" || len(result.Images) != 0 || !strings.Contains(result.Content, "invalid base64") {
				t.Fatalf("invalid image was not rejected: %+v", result)
			}
			return Message{ToolCalls: []ToolCall{memoryTestSaveCall(t, "procedure", "snapshot")}}, nil
		case 3:
			if result := messages[len(messages)-1]; !strings.Contains(result.Content, "successful tool evidence") {
				t.Fatalf("invalid image supported a procedure: %+v", result)
			}
			return Message{Content: "The snapshot could not be inspected."}, nil
		case 4:
			if len(available) != 1 || available[0].Name != "memory_save" {
				t.Fatalf("expected memory review, got tools %+v", available)
			}
			var review struct {
				Observations []memoryObservation `json:"observations"`
			}
			if err := json.Unmarshal([]byte(messages[1].Content), &review); err != nil {
				t.Fatal(err)
			}
			if len(review.Observations) != 1 || review.Observations[0].Succeeded || !strings.Contains(review.Observations[0].Output, "invalid base64") {
				t.Fatalf("learning did not receive the validated failure: %+v", review.Observations)
			}
			return Message{ToolCalls: []ToolCall{memoryTestSaveCall(t, "procedure", "snapshot")}}, nil
		default:
			t.Fatal("unexpected extra model request")
			return Message{}, nil
		}
	})
	engine := NewEngine(provider, tools, "Wendy")
	if err := engine.Turn(context.Background(), "Inspect the greeting", nil, func(context.Context, ToolCall) (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if requests != 4 {
		t.Fatalf("requests = %d, want normal task and memory review", requests)
	}
	entries, err := store.Search(context.Background(), "", "", 8)
	if err != nil || len(entries) != 0 {
		t.Fatalf("saved a procedure from rejected media: %+v, %v", entries, err)
	}
}

func TestMemorySearchHistoryReflectsCurrentNotesAndEnabledState(t *testing.T) {
	for _, change := range []string{"updated", "deleted", "disabled"} {
		t.Run(change, func(t *testing.T) {
			base := &engineTestExecutor{tools: []Tool{{Name: "probe"}}}
			tools, store := memoryTestTools(t, base)
			input := MemoryInput{Scope: "workspace", Kind: "fact", Title: "Paw API", Content: "original-diagnostic-marker", Evidence: "User supplied the API name"}
			entry, err := store.Save(context.Background(), input)
			if err != nil {
				t.Fatal(err)
			}
			var engine *Engine
			base.run = func(ctx context.Context, _ ToolCall) (string, error) {
				switch change {
				case "updated":
					input.Content = "corrected-diagnostic-marker"
					_, err := store.Save(ctx, input)
					return "probe complete", err
				case "deleted":
					return "probe complete", engine.ForgetMemory(ctx, entry.ID)
				default:
					engine.SetMemoryEnabled(false)
					return "probe complete", nil
				}
			}
			requests := 0
			provider := engineTestProvider(func(_ context.Context, messages []Message, available []Tool, _ func(string)) (Message, error) {
				if len(available) == 1 && available[0].Name == "memory_save" {
					return Message{}, nil
				}
				requests++
				if requests == 1 {
					return Message{ToolCalls: []ToolCall{{ID: "recall", Name: "memory_search", Arguments: json.RawMessage(`{"query":"Paw"}`)}}}, nil
				}
				var result *Message
				for i := range messages {
					if messages[i].Role == "tool" && messages[i].ToolCallID == "recall" {
						result = &messages[i]
					}
				}
				if result == nil {
					t.Fatal("refresh removed the tool result required by the provider protocol")
				}
				if requests == 2 {
					if !strings.Contains(result.Content, "original-diagnostic-marker") {
						t.Fatalf("initial search lost its result: %s", result.Content)
					}
					return Message{ToolCalls: []ToolCall{{ID: "probe", Name: "probe", Arguments: json.RawMessage(`{}`)}}}, nil
				}
				if requests != 3 {
					t.Fatalf("unexpected model request %d", requests)
				}
				if strings.Contains(result.Content, "original-diagnostic-marker") {
					t.Fatalf("stale memory search result sent after %s: %s", change, result.Content)
				}
				if change == "updated" && !strings.Contains(result.Content, "corrected-diagnostic-marker") {
					t.Fatalf("search history did not refresh corrected note: %s", result.Content)
				}
				if change != "updated" && strings.Contains(result.Content, entry.ID) {
					t.Fatalf("unavailable note still sent to provider: %s", result.Content)
				}
				return Message{Content: "Ready."}, nil
			})
			engine = NewEngine(provider, tools, "Wendy")
			if err := engine.Turn(context.Background(), "Check the notes", nil, nil); err != nil {
				t.Fatal(err)
			}
			if requests != 3 {
				t.Fatalf("model requests = %d, want 3", requests)
			}
		})
	}
}

func TestMemoryForgetCancelsOutstandingLearningAndRejectsStaleSave(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	base := &engineTestExecutor{tools: []Tool{{Name: "greet"}}}
	tools, store := memoryTestTools(t, base)
	saveCall := memoryTestSaveCall(t, "procedure", "greet")
	var input MemoryInput
	if err := json.Unmarshal(saveCall.Arguments, &input); err != nil {
		t.Fatal(err)
	}
	entry, err := store.Save(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	requests := 0
	provider := engineTestProvider(func(reviewCtx context.Context, _ []Message, _ []Tool, _ func(string)) (Message, error) {
		requests++
		switch requests {
		case 1:
			return Message{ToolCalls: []ToolCall{{ID: "greet", Name: "greet", Arguments: json.RawMessage(`{}`)}}}, nil
		case 2:
			return Message{Content: "The greeting command completed."}, nil
		case 3:
			started <- reviewCtx
			// Return a stale save even after cancellation, as a provider may have
			// completed its response just as the user deleted the note.
			<-release
			return Message{ToolCalls: []ToolCall{saveCall}}, nil
		default:
			return Message{}, fmt.Errorf("unexpected request %d", requests)
		}
	})
	engine := NewEngine(provider, tools, "Wendy")
	done := make(chan error, 1)
	go func() { done <- engine.Turn(ctx, "Do a paw greeting", nil, nil) }()
	var reviewCtx context.Context
	select {
	case reviewCtx = <-started:
	case <-ctx.Done():
		t.Fatal("learning did not start")
	}
	forgot := make(chan error, 1)
	go func() { forgot <- engine.ForgetMemory(ctx, entry.ID) }()
	select {
	case err := <-forgot:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("forget blocked on the outstanding learning request")
	}
	if reviewCtx.Err() == nil {
		t.Error("forgetting a note did not cancel the outstanding learning request")
	}
	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("canceling learning changed the completed task result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("turn did not finish after the stale learning response")
	}
	if _, err := tools.Execute(ctx, saveCall); err == nil {
		t.Error("accepted a save for a note forgotten during this turn")
	}
	entries, err := store.Search(ctx, "", "", 8)
	if err != nil || len(entries) != 0 {
		t.Fatalf("deleted note was resurrected: %+v, %v", entries, err)
	}
}
