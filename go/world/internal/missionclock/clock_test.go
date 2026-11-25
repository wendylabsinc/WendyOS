package missionclock

import (
	"strings"
	"testing"
	"time"
)

func sample() Observation {
	return Observation{RequestSHA256: strings.Repeat("a", 64), Tenant: "t", Site: "s", Audience: "a", RunID: "r", ClockDomain: "simulation:r", Purpose: "mission", Nonce: strings.Repeat("b", 64), OperatorPrincipal: "operator", OperatorKeySHA256: strings.Repeat("c", 64), GatewayPrincipal: "gateway", GatewayBootID: strings.Repeat("d", 64), SampleUnixMS: 10000000, Reference: Reference, MaxRTTMS: 2000, MaxHoldoverMS: 5000, DriftPPM: 100, QuantizationMS: 1}
}
func TestMissionEstimateRoundingAndProperty(t *testing.T) {
	for _, tt := range []struct {
		r, e         time.Duration
		lower, upper int64
	}{{0, 0, 9999999, 10000001}, {123456789, 1234567890, 10001232, 10001361}, {MaxRTT, MaxHoldover, 10004998, 10007002}} {
		interval, err := Estimate(sample(), tt.r, tt.e)
		if err != nil || interval.LowerUnixMS != tt.lower || interval.UpperUnixMS != tt.upper {
			t.Fatalf("%+v %v", interval, err)
		}
	}
	for r := time.Duration(0); r <= MaxRTT; r += 137 * time.Millisecond {
		for e := time.Duration(0); e <= MaxHoldover; e += 113 * time.Millisecond {
			v, err := Estimate(sample(), r, e)
			if err != nil {
				t.Fatal(err)
			}
			// The true gateway sample occurs anywhere in the measured RTT; bounds
			// contain both extremes, including the configured drift and quantization.
			if v.LowerUnixMS > sample().SampleUnixMS+int64(e/time.Millisecond) || v.UpperUnixMS < sample().SampleUnixMS+int64((r+e)/time.Millisecond) {
				t.Fatal("interval excluded possible true time")
			}
		}
	}
	for _, v := range [][2]time.Duration{{-1, 0}, {0, -1}, {MaxRTT + 1, 0}, {0, MaxHoldover + 1}, {time.Duration(1<<63 - 1), 0}} {
		if _, err := Estimate(sample(), v[0], v[1]); err == nil {
			t.Fatal("unbounded clock accepted")
		}
	}
	o := sample()
	o.Purpose = "annotation"
	if _, err := Estimate(o, 0, 0); err == nil {
		t.Fatal("wrong purpose")
	}
}
