package cloudrelay

import (
	"context"
	"testing"
	"time"
)

func TestCachedPresenceRefreshesBeforeSlowReconnect(t *testing.T) {
	a, r, _ := publicTransportFixture(t, 0, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.keys(); err != nil {
		t.Fatal(err)
	}
	l, c, err := a.issue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Still valid under the old 30s cutoff, but insufficient for a slow
	// connection plus admission. Keep a genuine Cloud-signed cached lease.
	c.Exp = time.Now().Add(60 * time.Second).Unix()
	l.PresenceLeaseJws = r.artifact(c, leaseType)
	a.lease, a.leaseClaims = l, c
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() { defer close(exited); done <- a.runOnce(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-exited:
		case <-time.After(time.Second):
			t.Error("presence did not stop")
		}
	})
	select {
	case <-r.present:
	case err := <-done:
		t.Fatalf("presence failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Stop before inspecting controller-owned state.
	cancel()
	<-done
	<-exited
	r.mu.Lock()
	issues := r.issues
	r.mu.Unlock()
	if issues != 2 || a.leaseClaims.JTI == c.JTI || time.Until(time.Unix(a.leaseClaims.Exp, 0)) < 4*time.Minute {
		t.Fatalf("reconnected with a nearly expired lease: issues=%d remaining=%v", issues, time.Until(time.Unix(a.leaseClaims.Exp, 0)))
	}
}
