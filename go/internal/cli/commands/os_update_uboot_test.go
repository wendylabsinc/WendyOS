package commands

import (
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestOSUpdateOutcomeRequiresUBootConfirmation(t *testing.T) {
	const version = "WendyOS-0.19.3"
	now := time.Now()
	for _, tc := range []struct {
		name        string
		currentSlot string
		bootSlot    string
		trial       string
		wantOK      bool
	}{
		{"confirmed B", "B", "1", "0", true},
		{"confirmed A", "A", "0", "0", true},
		{"trial still armed", "B", "1", "1", false},
		{"next boot selects previous slot", "B", "0", "0", false},
		{"missing diagnostics", "B", "", "", false},
		{"missing trial flag", "B", "1", "", false},
		{"missing selected slot", "B", "", "0", false},
		{"unknown trial flag", "B", "1", "unknown", false},
		{"unknown selected slot", "B", "B", "0", false},
		{"unknown running slot", "", "0", "0", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, receipt := range []string{"missing", "stale", "committed"} {
				t.Run(receipt, func(t *testing.T) {
					engine := &agentpb.OSUpdateEngineStatus{
						Connector: "ubootenv", CurrentSlot: tc.currentSlot,
						Slots: []*agentpb.OSUpdateEngineStatus_Slot{
							{Slot: "A", Booted: tc.currentSlot == "A"},
							{Slot: "B", Booted: tc.currentSlot == "B"},
						},
						Diagnostics: map[string]string{},
					}
					if tc.bootSlot != "" {
						engine.Diagnostics["wendyos_boot_slot"] = tc.bootSlot
					}
					if tc.trial != "" {
						engine.Diagnostics["wendyos_upgrade_available"] = tc.trial
					}
					if tc.trial == "1" {
						engine.Slots[1].Note = "trial armed (bootcount 1)"
					}
					resp := &agentpb.GetOSUpdateStatusResponse{EngineStatus: engine}
					baseline := osUpdateBaseline{bootedSlot: "A"}
					if tc.currentSlot == "A" {
						baseline.bootedSlot = "B"
					}
					if receipt != "missing" {
						resp.HasResult = true
						resp.Outcome = agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED
						resp.OldOsVersion, resp.NewOsVersion = version, version
						resp.CreatedAtUnix = now.Unix()
						if receipt == "stale" {
							baseline.resultAtUnix = resp.CreatedAtUnix
						}
					}
					msg, err := evaluateOSUpdateOutcomeSince(resp, nil, version, version, baseline, now)
					if (err == nil) != tc.wantOK {
						t.Fatalf("message=%q error=%v; want success=%v", msg, err, tc.wantOK)
					}
				})
			}
		})
	}
}
