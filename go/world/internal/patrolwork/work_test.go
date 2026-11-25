package patrolwork

import (
	"encoding/json"
	"strings"
	"testing"
)

func sample() Sample {
	return Sample{Schema: Schema, Method: Method, RunID: "run", BootID: "boot", ModelSHA256: strings.Repeat("a", 64), Sequence: 500, SimulationNS: 1e9, CapturedUnixMS: 1000, TotalJ: 50, PowerW: 20, Valid: true}
}
func TestLedgerConsumptionHoldRecoveryAndNoRefund(t *testing.T) {
	s := sample()
	l, err := New(Policy{100, 30}, s, 1000, s.RunID, s.ModelSHA256)
	if err != nil {
		t.Fatal(err)
	}
	s.Sequence++
	s.SimulationNS += 2e6
	s.CapturedUnixMS = 1100
	s.TotalJ = 125
	l.Update(&s, 1100)
	if !l.ReserveReached() || l.UsedJ != 75 || l.RemainingJ != 25 {
		t.Fatal(l)
	}
	l.Update(&s, 2200)
	if l.Valid || l.UsedJ != 75 || l.Permanent {
		t.Fatal(l)
	}
	s.Sequence++
	s.SimulationNS += 2e6
	s.CapturedUnixMS = 2300
	s.TotalJ = 130
	l.Update(&s, 2300)
	if !l.Valid || l.UsedJ != 80 || l.Baseline.TotalJ != 50 {
		t.Fatal(l)
	}
	s.Sequence++
	s.SimulationNS += 2e6
	s.CapturedUnixMS = 2400
	s.TotalJ = 151
	l.Update(&s, 2400)
	if l.Valid || !l.Exhausted || !l.Permanent || l.RemainingJ != 0 || l.UsedJ != 101 {
		t.Fatal(l)
	}
	s = sample()
	l.Update(&s, 2500)
	if !l.Exhausted || l.Baseline.TotalJ != 50 || l.UsedJ != 101 {
		t.Fatal("exhaustion restored", l)
	}
}
func TestResetReplayAndScopePermanentlyInvalidate(t *testing.T) {
	for _, mode := range []string{"run", "boot", "model", "method", "clock", "counter", "sequence", "mutated-duplicate", "same-clock", "wall-rollback"} {
		t.Run(mode, func(t *testing.T) {
			s := sample()
			l, _ := New(Policy{100, 0}, s, 1000, s.RunID, s.ModelSHA256)
			bad := s
			switch mode {
			case "run":
				bad.RunID = "other"
			case "boot":
				bad.BootID = "restart"
			case "model":
				bad.ModelSHA256 = strings.Repeat("b", 64)
			case "method":
				bad.Method = "other"
			case "clock":
				bad.SimulationNS--
			case "counter":
				bad.TotalJ--
			case "sequence":
				bad.Sequence--
			case "mutated-duplicate":
				bad.TotalJ++
			case "same-clock":
				bad.Sequence++
			case "wall-rollback":
				bad.CapturedUnixMS--
			}
			l.Update(&bad, 1100)
			if !l.Permanent || l.Valid {
				t.Fatal(l)
			}
			s.Sequence++
			s.SimulationNS += 2e6
			s.CapturedUnixMS = 1200
			l.Update(&s, 1200)
			if !l.Permanent || l.Valid || l.Baseline.TotalJ != 50 {
				t.Fatal(l)
			}
		})
	}
}
func TestUnknownAndFreshnessAreNotZero(t *testing.T) {
	s := sample()
	l, _ := New(Policy{100, 0}, s, 1000, s.RunID, s.ModelSHA256)
	l.Update(nil, 1001)
	if l.Valid || l.RemainingJ != 100 || l.Last.TotalJ != 50 {
		t.Fatal(l)
	}
	empty := Sample{}
	l.Update(&empty, 1002)
	if l.Valid || l.Permanent {
		t.Fatal("unknown sample fabricated reset", l)
	}
	l.Update(&s, 1002)
	if !l.Valid {
		t.Fatal(l)
	}
	future := s
	future.Sequence++
	future.SimulationNS += 2e6
	future.CapturedUnixMS = 1200
	l.Update(&future, 1002)
	if l.Valid {
		t.Fatal("future sample fresh")
	}
	if _, err := New(Policy{100, 0}, Sample{}, 1000, s.RunID, s.ModelSHA256); err == nil {
		t.Fatal("unknown baseline accepted")
	}
}
func TestPolicyRequiresExplicitBoundedNumbers(t *testing.T) {
	for _, raw := range []string{`{}`, `{"limit_j":null,"return_reserve_j":0}`, `{"limit_j":100}`, `{"limit_j":100,"return_reserve_j":null}`, `{"limit_j":100,"return_reserve_j":0,"battery":1}`} {
		var p Policy
		if json.Unmarshal([]byte(raw), &p) == nil {
			t.Fatal(raw)
		}
	}
	for _, p := range []Policy{{0, 0}, {-1, 0}, {100, 100}, {100, -1}, {1e10, 0}} {
		if p.Validate(true) == nil {
			t.Fatal(p)
		}
	}
	if (Policy{100, 30}).Validate(false) == nil {
		t.Fatal("reserve without return")
	}
	if (Policy{100, 0}).Validate(false) != nil {
		t.Fatal("allowance without reserve requires return")
	}
}

func TestSampleMissingMeasurementsAreUnknown(t *testing.T) {
	raw, _ := json.Marshal(sample())
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	for _, key := range []string{"total_j", "power_w", "sequence", "captured_unix_ms", "simulation_ns", "valid"} {
		copy := map[string]any{}
		for k, v := range value {
			copy[k] = v
		}
		delete(copy, key)
		raw, _ = json.Marshal(copy)
		var s Sample
		if json.Unmarshal(raw, &s) == nil {
			t.Fatal("omitted", key)
		}
		copy[key] = nil
		raw, _ = json.Marshal(copy)
		if json.Unmarshal(raw, &s) == nil {
			t.Fatal("null", key)
		}
	}
}
