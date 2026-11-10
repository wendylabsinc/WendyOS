package backend

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"testing"
)

func TestLocalResetRejectsStaleSession(t *testing.T) {
	b := New()
	b.conn = &grpcclient.AgentConnection{}
	b.session = 7
	b.life, b.stop = context.WithCancel(context.Background())
	defer b.stop()
	for _, session := range []uint64{0, 6, 8} {
		if _, err := b.PrepareLocalReset(session, "account", "org", "device"); err == nil {
			t.Fatal("accepted stale preflight")
		}
		if _, err := b.ResetLocalEnrollment(session, "account", "org", "device", "principal", "fingerprint"); err == nil {
			t.Fatal("accepted stale reset")
		}
	}
}
