package backend

import (
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"testing"
)

func TestAutomationRejectsMissingAndStaleSessions(t *testing.T) {
	b := webBackend(t)
	for _, session := range []uint64{0, 999} {
		if _, err := b.Pairings(session, false); err == nil {
			t.Fatal("pairing scan accepted stale session")
		}
		if err := b.PairSensor(session, "camera", false); err == nil {
			t.Fatal("pairing accepted stale session")
		}
		if err := b.PairIPCamera(session, 1, "admin", "secret", false); err == nil {
			t.Fatal("credentials accepted stale session")
		}
		if _, err := b.Campaigns(session); err == nil {
			t.Fatal("campaigns accepted stale session")
		}
		if _, err := b.CampaignSources(session); err == nil {
			t.Fatal("sources accepted stale session")
		}
		if err := b.DeployCampaign(session, commands.DesktopCampaignInput{}); err == nil {
			t.Fatal("deploy accepted stale session")
		}
	}
}
