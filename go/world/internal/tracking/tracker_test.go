package tracking

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func config() Config {
	return Config{TrackerID: "hub-session", RunID: "office", ClockDomain: "simulation:office", Frame: world.FrameRef{ID: "floor", Revision: "1"}}
}
func detection(source string, sequence uint64, member uint32, at float64, x, z float64) Detection {
	c := config()
	captured := start.Add(time.Duration(at * float64(time.Second)))
	return Detection{Source: source, SampleID: fmt.Sprintf("%s/%d", source, sequence), Version: world.Version{Epoch: 1, Sequence: sequence}, MemberIndex: member, SampleTime: captured, ValidUntil: captured.Add(2 * time.Second), RunID: c.RunID, ClockDomain: c.ClockDomain, Frame: c.Frame, PositionXZ: [2]float64{x, z}, Covariance: [4]float64{.04, 0, 0, .04}, ModelID: "pose-fixture", ModelVersion: "1", ModelSHA256: strings.Repeat("a", 64), Evidence: []sensing.EvidenceRef{{SHA256: strings.Repeat("b", 64), ContentType: "image/png", Size: 123}}}
}
func tracker(t *testing.T) *Tracker {
	t.Helper()
	tr, e := New(config())
	if e != nil {
		t.Fatal(e)
	}
	return tr
}
func step(t *testing.T, tr *Tracker, at float64, d ...Detection) []Track {
	t.Helper()
	out, e := tr.Step(start.Add(time.Duration(at*float64(time.Second))), d)
	if e != nil {
		t.Fatal(e)
	}
	return out
}

func TestCrossSourceFusionDeterministicAndCorrelationConservative(t *testing.T) {
	a, b := detection("camera-a", 1, 0, 0, 0, 0), detection("camera-b", 1, 0, 0, .2, 0)
	forward := step(t, tracker(t), 0, a, b)
	reverse := step(t, tracker(t), 0, b, a)
	if !reflect.DeepEqual(forward, reverse) || len(forward) != 1 {
		t.Fatalf("order affected fusion: %+v %+v", forward, reverse)
	}
	got := forward[0]
	if math.Abs(got.PositionXZ[0]-.1) > 1e-12 || math.Abs(got.Covariance[0]-.04) > 1e-12 || len(got.Members) != 2 {
		t.Fatalf("bad CI: %+v", got)
	}
	if got.Members[0].SampleID != a.SampleID || got.Members[0].ModelSHA256 != a.ModelSHA256 || got.Members[0].CalibrationRevision != a.CalibrationRevision || got.Members[1].Evidence[0] != b.Evidence[0] {
		t.Fatal("lost original lineage")
	}
	encoded, e := json.Marshal(got)
	if e != nil || !strings.Contains(string(encoded), Schema) {
		t.Fatal("track JSON unavailable", e)
	}
}

func TestSkewUsesLatestTimeWithoutChangingOriginalMeasurements(t *testing.T) {
	a, b := detection("a", 1, 0, 0, 0, 0), detection("b", 1, 0, .2, .2, 0)
	a.CalibrationRevision = "camera-a-calibration-3"
	out := step(t, tracker(t), .2, a, b)
	if len(out) != 1 || !out[0].LastObserved.Equal(b.SampleTime) || out[0].PositionXZ[0] < .15 {
		t.Fatalf("older measurement over-weighted: %+v", out)
	}
	if !reflect.DeepEqual(out[0].Members, []Detection{a, b}) {
		t.Fatal("skew alignment rewrote original measurement or calibration")
	}
	// Independent closed-form CI expectation for complementary uncertainty.
	p, cov := intersection([2]float64{}, [2]float64{}, [4]float64{.01, 0, 0, 1}, [4]float64{1, 0, 0, .01})
	want := 1 / (.5/.01 + .5/1)
	if p != [2]float64{} || math.Abs(cov[0]-want) > 1e-12 || math.Abs(cov[3]-want) > 1e-12 {
		t.Fatalf("unexpected CI optimum %v", cov)
	}
}

func TestSameCameraMembersAndTransitiveBridgeRemainSeparate(t *testing.T) {
	t.Run("same camera", func(t *testing.T) {
		out := step(t, tracker(t), 0, detection("camera-a", 1, 0, 0, 0, 0), detection("camera-a", 1, 1, 0, .1, 0))
		if len(out) != 2 {
			t.Fatalf("merged same camera: %+v", out)
		}
	})
	t.Run("transitive chain", func(t *testing.T) {
		out := step(t, tracker(t), 0, detection("camera-a", 1, 0, 0, 0, 0), detection("camera-b", 1, 0, 0, .5, 0), detection("camera-c", 1, 0, 0, 1, 0))
		if len(out) != 2 {
			t.Fatalf("joined separated endpoints: %+v", out)
		}
		for _, track := range out {
			if len(track.Members) > 2 {
				t.Fatal("transitive group")
			}
		}
	})
	t.Run("two people two cameras", func(t *testing.T) {
		out := step(t, tracker(t), 0, detection("camera-b", 1, 1, 0, 2.1, 0), detection("camera-a", 1, 0, 0, 0, 0), detection("camera-b", 1, 0, 0, .1, 0), detection("camera-a", 1, 1, 0, 2, 0))
		if len(out) != 2 || len(out[0].Members) != 2 || len(out[1].Members) != 2 {
			t.Fatalf("wrong pair grouping: %+v", out)
		}
	})
}

func TestCrossSourceSkewDistanceAndCovarianceGates(t *testing.T) {
	for name, change := range map[string]func(*Detection){"skew": func(d *Detection) { d.SampleTime = d.SampleTime.Add(351 * time.Millisecond) }, "distance": func(d *Detection) { d.PositionXZ[0] = .651 }, "covariance": func(d *Detection) { d.PositionXZ[0] = .6; d.Covariance = [4]float64{.0001, 0, 0, .0001} }} {
		t.Run(name, func(t *testing.T) {
			a, b := detection("a", 1, 0, 0, 0, 0), detection("b", 1, 0, 0, 0, 0)
			if name == "covariance" {
				a.Covariance = [4]float64{.0001, 0, 0, .0001}
			}
			change(&b)
			out := step(t, tracker(t), .4, a, b)
			if len(out) != 2 {
				t.Fatalf("gate ignored: %+v", out)
			}
		})
	}
}

func TestTemporalContinuityAndMotionGate(t *testing.T) {
	tr := tracker(t)
	first := step(t, tr, 0, detection("a", 1, 0, 0, 0, 0))[0]
	for n := 1; n <= 5; n++ {
		at := float64(n) * .2
		out := step(t, tr, at, detection("a", uint64(n+1), 0, at, at, 0))
		if len(out) != 1 || out[0].ID != first.ID || out[0].Revision != uint64(n+1) || out[0].VelocityXZ[0] <= 0 {
			t.Fatalf("lost temporal identity: %+v", out)
		}
	}
	out := step(t, tr, 1.1, detection("a", 7, 0, 1.1, 10, 0))
	if len(out) != 2 || out[1].ID == first.ID {
		t.Fatalf("impossible jump associated: %+v", out)
	}
}

func TestCoastingPredictionDoesNotAccumulateOrRefresh(t *testing.T) {
	setup := func() *Tracker {
		tr := tracker(t)
		step(t, tr, 0, detection("a", 1, 0, 0, 0, 0))
		step(t, tr, .5, detection("a", 2, 0, .5, .5, 0))
		return tr
	}
	a, b := setup(), setup()
	original := step(t, a, .5)[0]
	step(t, a, .6)
	step(t, a, .7)
	step(t, a, .8)
	aout, bout := step(t, a, 1), step(t, b, 1)
	if !reflect.DeepEqual(aout, bout) {
		t.Fatalf("snapshots recursively altered prediction: %+v %+v", aout, bout)
	}
	got := aout[0]
	if got.State != "coasting" || !got.LastObserved.Equal(original.LastObserved) || !got.ValidUntil.Equal(original.ValidUntil) || got.Revision != original.Revision {
		t.Fatalf("coasting refreshed measurement: %+v", got)
	}
	want := original.PositionXZ[0] + original.VelocityXZ[0]*.5
	if math.Abs(got.PositionXZ[0]-want) > 1e-12 {
		t.Fatalf("position %v want %v", got.PositionXZ, want)
	}
	if out := step(t, a, 2.5); len(out) != 0 {
		t.Fatal("track survived equal expiry")
	}
}

func TestDuplicateAndReorderedObservationsDoNotRefresh(t *testing.T) {
	tr := tracker(t)
	old := detection("a", 1, 0, 0, 0, 0)
	first := step(t, tr, 0, old, old)[0]
	again := step(t, tr, .5, old)[0]
	if again.Revision != first.Revision || again.State != "coasting" || !again.ValidUntil.Equal(first.ValidUntil) {
		t.Fatal("duplicate became fresh")
	}
	newer := detection("a", 2, 0, .6, .1, 0)
	current := step(t, tr, .6, newer)[0]
	got := step(t, tr, .8, old)[0]
	if got.Revision != current.Revision || !got.LastObserved.Equal(newer.SampleTime) {
		t.Fatal("older version changed current track")
	}
	if out := step(t, tr, 2.6, newer); len(out) != 0 {
		t.Fatal("repeated expiry refreshed track")
	}
	if out := step(t, tr, 2.7, old); len(out) != 0 {
		t.Fatal("old record resurrected track")
	}
}

func TestKnownOldGroupCannotStealNewTemporalAssignment(t *testing.T) {
	c := config()
	c.GateSquared = 100
	tr, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	a := detection("a", 1, 0, 0, 0, 0)
	first := step(t, tr, 0, a)[0]
	b := detection("b", 1, 0, .2, .7, 0)
	out := step(t, tr, .2, a, b)
	if len(out) != 1 || out[0].ID != first.ID || out[0].Revision != 2 || out[0].Members[0].Source != "b" {
		t.Fatalf("duplicate stole assignment: %+v", out)
	}
}

func TestAmbiguousCrossingsPreserveSeparateAnonymousTracks(t *testing.T) {
	tr := tracker(t)
	initial := step(t, tr, 0, detection("a", 1, 0, 0, -.25, 0), detection("a", 1, 1, 0, .25, 0))
	out := step(t, tr, .2, detection("a", 2, 0, .2, 0, 0), detection("a", 2, 1, .2, 0, 0))
	if len(out) != 2 || out[0].ID != initial[0].ID || out[1].ID != initial[1].ID || !out[0].Ambiguous || !out[1].Ambiguous {
		t.Fatalf("crossing collapsed or ambiguity hidden: %+v", out)
	}
	for _, v := range out {
		if len(v.Members) != 1 {
			t.Fatal("same-camera crossing fused")
		}
	}
	// Cross-camera matching can also be ambiguous before temporal tracks exist.
	groups := step(t, tracker(t), 0, detection("a", 1, 0, 0, -.1, 0), detection("a", 1, 1, 0, .1, 0), detection("b", 1, 0, 0, 0, 0))
	if len(groups) != 2 || !groups[0].Ambiguous {
		t.Fatalf("cross-source ambiguity hidden: %+v", groups)
	}
}

func TestOriginalShortestExpiryAndBoundedCoast(t *testing.T) {
	a, b := detection("a", 1, 0, 0, 0, 0), detection("b", 1, 0, 0, .1, 0)
	a.ValidUntil = start.Add(500 * time.Millisecond)
	tr := tracker(t)
	out := step(t, tr, 0, a, b)
	if !out[0].ValidUntil.Equal(a.ValidUntil) {
		t.Fatal("required member expiry extended")
	}
	if out := step(t, tr, .5, a, b); len(out) != 0 {
		t.Fatal("expired fused input became a fresh result")
	}
	d := detection("c", 1, 0, .6, 0, 0)
	d.ValidUntil = start.Add(time.Hour)
	out = step(t, tr, .6, d)
	if !out[0].ValidUntil.Equal(d.SampleTime.Add(2 * time.Second)) {
		t.Fatal("unbounded coast")
	}
}

func TestInvalidBatchAtomicAndExactModelLineage(t *testing.T) {
	changes := map[string]func(*Detection){"frame": func(d *Detection) { d.Frame.Revision = "2" }, "run": func(d *Detection) { d.RunID = "other" }, "clock": func(d *Detection) { d.ClockDomain = "wall" }, "model digest": func(d *Detection) { d.ModelSHA256 = "" }, "singular": func(d *Detection) { d.Covariance = [4]float64{1, 1, 1, 1} }, "asymmetric": func(d *Detection) { d.Covariance[1] = .1 }, "NaN": func(d *Detection) { d.PositionXZ[0] = math.NaN() }, "future": func(d *Detection) { d.SampleTime = start.Add(time.Second) }}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			tr := tracker(t)
			d := detection("a", 1, 0, 0, 0, 0)
			change(&d)
			if _, e := tr.Step(start, []Detection{d}); e == nil {
				t.Fatal("accepted invalid detection")
			}
			out := step(t, tr, 0, detection("a", 1, 0, 0, 0, 0))
			if len(out) != 1 || out[0].ID != "hub-session/000001" {
				t.Fatal("refused batch changed tracker")
			}
		})
	}
	tr := tracker(t)
	d := detection("a", 1, 0, 0, 0, 0)
	step(t, tr, 0, d)
	d.PositionXZ[0] = .1
	if _, e := tr.Step(start.Add(time.Second), []Detection{d}); e == nil {
		t.Fatal("changed original accepted")
	}
	if out := step(t, tr, .5); out[0].Revision != 1 {
		t.Fatal("conflict changed state/time")
	}
}

func TestOwnershipAndBounds(t *testing.T) {
	tr := tracker(t)
	d := detection("a", 1, 0, 0, 0, 0)
	out := step(t, tr, 0, d)
	d.Evidence[0].SHA256 = strings.Repeat("c", 64)
	out[0].Members[0].PositionXZ[0] = 100
	out[0].Members[0].Evidence[0].Size = 999
	got := step(t, tr, .1)[0]
	if got.Members[0].PositionXZ[0] != 0 || got.Members[0].Evidence[0].Size != 123 || got.Members[0].Evidence[0].SHA256 != strings.Repeat("b", 64) {
		t.Fatal("caller mutated tracker")
	}
	c := config()
	c.MaxTracks = 1
	tr, e := New(c)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := tr.Step(start, []Detection{detection("a", 1, 0, 0, 0, 0), detection("a", 1, 1, 0, 3, 0)}); e == nil {
		t.Fatal("track bound ignored")
	}
	if step(t, tr, 0, detection("a", 1, 0, 0, 0, 0))[0].ID != "hub-session/000001" {
		t.Fatal("overflow partially committed")
	}
	c = config()
	c.MaxSources = 1
	tr, _ = New(c)
	if _, e := tr.Step(start, []Detection{detection("a", 1, 0, 0, 0, 0), detection("b", 1, 0, 0, 0, 0)}); e == nil {
		t.Fatal("source bound ignored")
	}
}
