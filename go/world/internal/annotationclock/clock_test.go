package annotationclock

import (
	"strings"
	"testing"
	"time"
)

func sample() Observation {
	return Observation{RequestSHA256: strings.Repeat("a", 64), Tenant: "t", Site: "s", Audience: "a", RunID: "r", ClockDomain: "simulation:r", Nonce: strings.Repeat("b", 64), OperatorPrincipal: "operator", OperatorKeySHA256: strings.Repeat("c", 64), GatewayPrincipal: "gateway", GatewayBootID: strings.Repeat("d", 64), SampleUnixMS: 10000000, Reference: Reference, MaxRTTMS: 2000, MaxHoldoverMS: 3600000, DriftPPM: 100, QuantizationMS: 1}
}
func TestEstimateOutwardRoundingAndBounds(t *testing.T) {
	for _, tt := range []struct {
		name         string
		rtt, elapsed time.Duration
		lo, hi       int64
	}{{"zero", 0, 0, 9999999, 10000001}, {"fractional", 123456789, 1234567890123, 11234442, 11234817}, {"maximum", MaxRTT, MaxHoldover, 13599638, 13602362}} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Estimate(sample(), tt.rtt, tt.elapsed)
			if err != nil || got.LowerUnixMS != tt.lo || got.UpperUnixMS != tt.hi {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	for _, v := range [][2]time.Duration{{-1, 0}, {0, -1}, {MaxRTT + 1, 0}, {0, MaxHoldover + 1}, {time.Duration(1<<63 - 1), 0}} {
		if _, err := Estimate(sample(), v[0], v[1]); err == nil {
			t.Fatal("unbounded interval accepted")
		}
	}
	changed := sample()
	changed.DriftPPM = 0
	if _, err := Estimate(changed, 0, 0); err == nil {
		t.Fatal("changed policy")
	}
}
