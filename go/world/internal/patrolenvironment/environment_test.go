package patrolenvironment

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"strings"
	"testing"
)

func policy() Policy {
	return Policy{SensorID: "gas-1", Calibration: "go2-virtual-mounts-v1", Species: "synthetic-test-gas", Unit: "ppm", HoldPPM: 80, ClearPPM: 65, ClearForMS: 1000}
}
func sample(seq uint64, value float64) Sample {
	return Sample{Schema: Schema, Method: Method, RunID: "run", BootID: "boot", ModelSHA256: strings.Repeat("a", 64), Frame: world.FrameRef{ID: "office-floor", Revision: "v1"}, SensorID: "gas-1", Calibration: "go2-virtual-mounts-v1", Species: "synthetic-test-gas", Unit: "ppm", Provenance: "synthetic", Sequence: seq, SimulationNS: int64(seq) * 100000000, CapturedUnixMS: 1000 + int64(seq)*100, ValuePPM: value, UncertaintyPPM: 1.5, Valid: true}
}
func ledger(t *testing.T) *Ledger {
	t.Helper()
	s := sample(1, 10)
	l, err := New(policy(), s.RunID, s.ModelSHA256, s.BootID, s.Frame)
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func feed(l *Ledger, from, to uint64, value float64) {
	for i := from; i <= to; i++ {
		s := sample(i, value)
		l.Update([]Sample{s}, s.CapturedUnixMS)
	}
}
func TestOriginalClearIntervalAndExplicitResume(t *testing.T) {
	l := ledger(t)
	feed(l, 1, 10, 10)
	if l.Ready || l.ClearElapsedMS != 900 {
		t.Fatal(l)
	}
	// Repeated HTTP reads cannot supply missing original time.
	s := sample(10, 10)
	l.Update([]Sample{s}, s.CapturedUnixMS+500)
	if l.Ready || l.ClearElapsedMS != 900 {
		t.Fatal(l)
	}
	s = sample(11, 10)
	s.CapturedUnixMS = 2500
	l.Update([]Sample{s}, 2500)
	if !l.Ready || !l.Held || l.Resume(2500) != nil || l.Held {
		t.Fatal(l)
	}
}
func TestRetainedSpikeAndHysteresis(t *testing.T) {
	l := ledger(t)
	feed(l, 1, 11, 10)
	if err := l.Resume(2100); err != nil {
		t.Fatal(err)
	}
	high := sample(12, 79)
	low := sample(13, 10)
	l.Update([]Sample{sample(11, 10), high, low}, 2300)
	if !l.Held || l.Ready || l.ClearSinceNS != low.SimulationNS {
		t.Fatal("between-poll spike was lost", l)
	}
	feed(l, 14, 20, 10)
	feed(l, 21, 21, 70)
	if l.ClearElapsedMS != 0 {
		t.Fatal("hysteresis did not reset clear interval")
	}
	feed(l, 22, 32, 10)
	if !l.Ready || !l.Held {
		t.Fatal(l)
	}
}
func TestAdmissionDoesNotCountPreMissionHistory(t *testing.T) {
	l := ledger(t)
	var batch []Sample
	for i := uint64(1); i <= 30; i++ {
		batch = append(batch, sample(i, 10))
	}
	l.Update(batch, 4000)
	if l.Ready || l.ClearElapsedMS != 0 || l.Last.Sequence != 30 {
		t.Fatal(l)
	}
}
func TestUnknownGapsReorderingAndStaleNeverClear(t *testing.T) {
	for _, mode := range []string{"missing", "stale", "sequence-gap", "time-gap", "reordered", "replay", "future", "invalid", "uncertainty"} {
		t.Run(mode, func(t *testing.T) {
			l := ledger(t)
			feed(l, 1, 11, 10)
			_ = l.Resume(2100)
			s := sample(12, 10)
			batch := []Sample{s}
			now := int64(2200)
			switch mode {
			case "missing":
				batch = nil
			case "stale":
				now = 4000
			case "sequence-gap":
				batch = []Sample{sample(20, 10)}
				now = 3000
			case "time-gap":
				batch[0].SimulationNS += 1000000000
			case "reordered":
				batch = []Sample{sample(13, 10), sample(12, 10)}
				now = 2300
			case "replay":
				batch = []Sample{sample(10, 10)}
			case "future":
				batch[0].CapturedUnixMS = 3000
			case "invalid":
				batch[0].Valid = false
			case "uncertainty":
				batch[0].UncertaintyPPM = 71
			}
			l.Update(batch, now)
			if !l.Held || l.Ready || l.Resume(now) == nil {
				t.Fatal(mode, l)
			}
		})
	}
}
func TestSourceResetAndEquivocationPermanent(t *testing.T) {
	for _, mode := range []string{"boot", "run", "frame", "model", "equivocation", "wall"} {
		t.Run(mode, func(t *testing.T) {
			l := ledger(t)
			feed(l, 1, 11, 10)
			_ = l.Resume(2100)
			s := sample(12, 10)
			now := int64(2200)
			switch mode {
			case "boot":
				s.BootID = "other"
			case "run":
				s.RunID = "other"
			case "frame":
				s.Frame.Revision = "other"
			case "model":
				s.ModelSHA256 = strings.Repeat("b", 64)
			case "equivocation":
				s = sample(11, 11)
			case "wall":
				now = 2000
			}
			l.Update([]Sample{s}, now)
			if !l.Permanent || !l.Held {
				t.Fatal(l)
			}
			feed(l, 13, 40, 10)
			if l.Ready || l.Resume(5000) == nil {
				t.Fatal("reset source restored")
			}
		})
	}
}
func TestMissingNullAndInvalidPolicies(t *testing.T) {
	p, _ := json.Marshal(policy())
	s, _ := json.Marshal(sample(1, 10))
	for _, raw := range [][]byte{p, s} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(raw, &fields)
		for key := range fields {
			for _, null := range []bool{false, true} {
				var copy map[string]json.RawMessage
				_ = json.Unmarshal(raw, &copy)
				if null {
					copy[key] = json.RawMessage("null")
				} else {
					delete(copy, key)
				}
				b, _ := json.Marshal(copy)
				var err error
				if string(raw) == string(p) {
					var v Policy
					err = json.Unmarshal(b, &v)
				} else {
					var v Sample
					err = json.Unmarshal(b, &v)
				}
				if err == nil {
					t.Fatalf("accepted missing/null %s", key)
				}
			}
		}
	}
	for _, change := range []func(*Policy){func(p *Policy) { p.ClearPPM = p.HoldPPM }, func(p *Policy) { p.ClearForMS = 999 }, func(p *Policy) { p.Species = "toxic-gas" }} {
		p := policy()
		change(&p)
		if p.Validate() == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}
