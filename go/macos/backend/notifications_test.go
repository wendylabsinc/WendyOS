package backend

import (
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"testing"
)

func TestNotificationSettingsRejectStaleDeviceSelection(t *testing.T) {
	b := New()
	b.session = 7
	b.conn = &grpcclient.AgentConnection{}
	for _, session := range []uint64{0, 6} {
		if _, err := b.CampaignNotifications(session); err == nil {
			t.Fatal("read accepted a stale device session")
		}
		if err := b.SetCampaignNotifications(session, "people", true); err == nil {
			t.Fatal("grant accepted a stale device session")
		}
	}
}
