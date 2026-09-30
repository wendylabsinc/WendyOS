package mcp

import (
	"context"
	"fmt"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"sync"
	"time"
)

const gatewayTaskStateKey = "wendy/internal-task-context"

type gatewayTaskState struct {
	ctx    context.Context
	cancel context.CancelFunc
}

func gatewayTaskContext(r mcpgo.CallToolRequest) (context.Context, error) {
	if r.Params.Meta != nil {
		if state, ok := r.Params.Meta.AdditionalFields[gatewayTaskStateKey].(*gatewayTaskState); ok {
			return state.ctx, nil
		}
	}
	return nil, fmt.Errorf("Missing task context")
}

// mcp-go 0.54 cancels each JSON-RPC request on return, including a task's
// creating request. Supply a bounded task lifetime and connect tasks/cancel to
// it through lifecycle hooks. No dependency fork or detached unbounded work.
func gatewayTaskHooks() (*server.Hooks, *server.TaskHooks) {
	hooks := &server.Hooks{}
	tasks := &server.TaskHooks{}
	var mu sync.Mutex
	pending := map[context.Context]*gatewayTaskState{}
	running := map[string]*gatewayTaskState{}
	hooks.AddBeforeCallTool(func(ctx context.Context, _ any, r *mcpgo.CallToolRequest) {
		if r.Params.Task == nil || (r.Params.Name != "wait_for_device_event" && r.Params.Name != "simulator_start" && r.Params.Name != "simulator_create") {
			return
		}
		duration := 5 * time.Minute
		if r.Params.Name != "wait_for_device_event" {
			duration = 30 * time.Minute
		}
		ttl := int64((duration + 5*time.Minute) / time.Millisecond)
		r.Params.Task.TTL = &ttl
		taskCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), duration)
		state := &gatewayTaskState{taskCtx, cancel}
		// Replace rather than accepting client-supplied internal metadata.
		meta := map[string]any{}
		if r.Params.Meta != nil {
			for k, v := range r.Params.Meta.AdditionalFields {
				meta[k] = v
			}
		}
		meta[gatewayTaskStateKey] = state
		r.Params.Meta = mcpgo.NewMetaFromMap(meta)
		mu.Lock()
		pending[ctx] = state
		mu.Unlock()
	})
	tasks.AddOnTaskCreated(func(ctx context.Context, m server.TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		if s := pending[ctx]; s != nil {
			running[m.TaskID] = s
			delete(pending, ctx)
		}
	})
	finish := func(_ context.Context, m server.TaskMetrics) {
		mu.Lock()
		defer mu.Unlock()
		if s := running[m.TaskID]; s != nil {
			s.cancel()
			delete(running, m.TaskID)
		}
	}
	tasks.AddOnTaskCancelled(finish)
	tasks.AddOnTaskCompleted(finish)
	tasks.AddOnTaskFailed(finish)
	// A rejected task request never emits a task lifecycle event.
	hooks.AddAfterCallTool(func(ctx context.Context, _ any, _ *mcpgo.CallToolRequest, _ any) {
		mu.Lock()
		defer mu.Unlock()
		if s := pending[ctx]; s != nil {
			s.cancel()
			delete(pending, ctx)
		}
	})
	hooks.AddOnError(func(ctx context.Context, _ any, _ mcpgo.MCPMethod, _ any, _ error) {
		mu.Lock()
		defer mu.Unlock()
		if s := pending[ctx]; s != nil {
			s.cancel()
			delete(pending, ctx)
		}
	})
	return hooks, tasks
}
