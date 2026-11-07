package backend

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"testing"
)

func TestDeviceManagementRejectsStaleSessionBeforeRPC(t *testing.T) {
	b := New()
	b.conn = &grpcclient.AgentConnection{}
	b.session = 7
	b.life, b.stop = context.WithCancel(context.Background())
	defer b.stop()
	for _, session := range []uint64{0, 6, 8} {
		for _, remove := range []bool{false, true} {
			if err := b.ManageDevice(session, "account", "org", "device", "name", remove); err == nil {
				t.Fatalf("accepted session %d", session)
			}
		}
	}
}
