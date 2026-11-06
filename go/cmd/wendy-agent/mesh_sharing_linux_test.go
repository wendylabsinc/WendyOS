//go:build linux

package main

import (
	"context"
	"errors"
	"testing"
)

type failingSharingCleanup struct {
	err   error
	calls int
}

func (c *failingSharingCleanup) Close() error {
	c.calls++
	return c.err
}

func TestMeshSharingDeactivateRetainsCleanupFailure(t *testing.T) {
	want := errors.New("resolver restore failed")
	cleanup := &failingSharingCleanup{err: want}
	m := &meshSharingManager{}
	if err := m.closeController(cleanup); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if err := m.Deactivate(); !errors.Is(err, want) {
		t.Fatalf("reset could ignore failed cleanup: %v", err)
	}
	if m.pendingCleanup != cleanup || m.state.State != "error" {
		t.Fatalf("lost failed cleanup owner/state: %+v", m.state)
	}
	// Re-enabling mesh must not replace the pending owner with a new node.
	m.Activate(context.Background(), nil)
	if m.done != nil || m.pendingCleanup != cleanup || cleanup.calls != 3 {
		t.Fatal("activation bypassed pending cleanup")
	}
	cleanup.err = nil
	if err := m.Deactivate(); err != nil {
		t.Fatal(err)
	}
	if m.pendingCleanup != nil || m.state.State != "" {
		t.Fatal("successful retry did not clear cleanup error")
	}
	if err := m.Deactivate(); err != nil || cleanup.calls != 4 {
		t.Fatalf("completed cleanup was not idempotent: calls=%d err=%v", cleanup.calls, err)
	}
}
