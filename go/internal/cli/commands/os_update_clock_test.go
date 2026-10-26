package commands

import (
	"testing"
	"time"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestOSUpdateBaselinePreservesClearedReceiptWithSkewedClock(t *testing.T) {
	now := time.Now()
	const version = "WendyOS-0.19.4"
	for _, tc := range []struct {
		name       string
		cleared    bool
		oldVersion string
		previous   int64
		result     int64
		want       bool
	}{
		{"offline clock", true, version, 50, 100, true},
		{"clock moved backwards", true, version, 200, 100, true},
		{"unchanged receipt", true, version, 100, 100, false},
		{"receipt from another update", true, "WendyOS-0.18.0", 50, 100, false},
		{"old agent cannot establish freshness", false, version, 50, 100, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseline := osUpdateBaseline{recordCleared: tc.cleared, preVersion: version, resultAtUnix: tc.previous}
			resp := &agentpb.GetOSUpdateStatusResponse{HasResult: true, Outcome: agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED,
				OldOsVersion: tc.oldVersion, NewOsVersion: version, CreatedAtUnix: tc.result}
			if got := usableOSUpdateOutcomeSince(resp, now, baseline); got != tc.want {
				t.Fatalf("usable = %v, want %v", got, tc.want)
			}
			_, err := evaluateOSUpdateOutcomeSince(resp, nil, version, version, baseline, now)
			if (err == nil) != tc.want {
				t.Fatalf("evaluation error = %v; want verified=%v", err, tc.want)
			}
		})
	}
}
