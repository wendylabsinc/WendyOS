package taskrelay

import (
	"context"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"testing"
)

func TestTraceStatusStillChecksCurrentAuthorityAndPersists(t *testing.T) {
	f := newFixture(t)
	in := f.instruction("traced", "mission")
	status := f.status(in, 1, mission.Accepted)
	c, err := diagnostics.New("0123456789abcdef0123456789abcdef", diagnostics.Limits{WindowMS: 3000, GraceMS: 3000, MaxRequests: 8, MaxEvents: 128})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Arm("0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	ctx, request := c.Start(context.Background(), diagnostics.TaskRelayRound)
	if _, err = f.store.PutInstructionContext(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.PutStatusContext(ctx, status); err != nil {
		t.Fatal(err)
	}
	request.Finish(0)
	snap := c.Snapshot()
	holds := 0
	for _, e := range snap.Events {
		if e.RequestID != 1 || e.EndNS < 0 {
			t.Fatal(e)
		}
		if e.Phase == "verifier_hold" {
			holds++
		}
	}
	if holds < 2 {
		t.Fatal("nested current verification lost correlation", holds)
	}
	f.reopen()
	entries, err := f.store.List()
	if err != nil || len(entries) != 1 || entries[0].Status == nil {
		t.Fatal(entries, err)
	}
	f.revocations(2, []string{f.device.Grant.Principal})
	ctx, deniedRequest := c.Start(context.Background(), diagnostics.TaskRelayRound)
	defer deniedRequest.Finish(0)
	if _, err = f.store.PutStatusContext(ctx, status); err == nil {
		t.Fatal("context bypassed revoked status")
	}
}
