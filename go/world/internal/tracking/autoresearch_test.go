package tracking

import (
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

// Truth is used only by the scorer. Step receives calibrated camera detections.
// The fixed seeds make candidate and baseline scores directly comparable. These
// synthetic scenarios are regression fixtures, not a real-world validation set.
type fusionResearchFrame struct {
	at         time.Time
	truth      [][2]float64
	detections []Detection
}
type fusionResearchScene struct {
	name   string
	frames []fusionResearchFrame
}

func fusionResearchScenes() []fusionResearchScene {
	var scenes []fusionResearchScene
	for _, seed := range []int64{17, 491, 2027} {
		for _, crossing := range []bool{false, true} {
			rng := rand.New(rand.NewSource(seed))
			scene := fusionResearchScene{name: fmt.Sprintf("crossing=%t/seed=%d", crossing, seed)}
			position := func(actor int, seconds float64) [2]float64 {
				if crossing {
					if actor == 0 {
						return [2]float64{-2 + .5*seconds, .12}
					}
					return [2]float64{2 - .5*seconds, -.12}
				}
				return [2]float64{float64(actor)*1.8 + .35*seconds, .3 * math.Sin(seconds*.8+float64(actor))}
			}
			for frame := 0; frame < 80; frame++ {
				seconds := float64(frame) * .1
				f := fusionResearchFrame{at: start.Add(time.Duration(frame) * 100 * time.Millisecond)}
				for actor := 0; actor < 2; actor++ {
					f.truth = append(f.truth, position(actor, seconds))
				}
				for camera := 0; camera < 3; camera++ {
					// An entire camera drops out, and one person is occluded in
					// every camera for 700 ms. Both recover before input expiry.
					if camera == 1 && frame >= 20 && frame < 45 {
						continue
					}
					for actor := 0; actor < 2; actor++ {
						if actor == 0 && frame >= 48 && frame < 55 {
							continue
						}
						if rng.Float64() < .08 && frame > 0 {
							continue
						}
						skew := float64(camera) * .035
						p := position(actor, seconds-skew)
						sx, sz := .08+.025*float64(camera), .11-.02*float64(camera)
						p[0] += rng.NormFloat64() * sx
						p[1] += rng.NormFloat64() * sz
						// Member indices change between samples, so they cannot
						// act as persistent ground-truth person identities.
						d := detection(fmt.Sprintf("camera-%d", camera), uint64(frame+1), uint32((actor+frame)%2), seconds-skew, p[0], p[1])
						d.Covariance = [4]float64{sx * sx, 0, 0, sz * sz}
						d.ValidUntil = d.SampleTime.Add(1500 * time.Millisecond)
						d.CalibrationRevision = fmt.Sprintf("camera-%d-calibration-1", camera)
						f.detections = append(f.detections, d)
					}
				}
				scene.frames = append(scene.frames, f)
			}
			scenes = append(scenes, scene)
		}
	}
	return scenes
}

// Minimum assignment cost includes a 2 m cost for each missing or extra track.
// Capping position distance at 2 m bounds outliers. An extra track never provides
// a free extra guess. The DP is linear in track count for these two-person scenes.
func fusionResearchError(truth [][2]float64, tracks []Track) float64 {
	const penalty = 2.0
	n := 1 << len(truth)
	costs := make([]float64, n)
	for mask := 1; mask < n; mask++ {
		costs[mask] = math.Inf(1)
	}
	for _, tr := range tracks {
		next := make([]float64, n)
		for mask := range next {
			next[mask] = costs[mask] + penalty
		}
		for mask, cost := range costs {
			for actor, p := range truth {
				bit := 1 << actor
				if mask&bit != 0 {
					continue
				}
				d := math.Min(penalty, math.Hypot(p[0]-tr.PositionXZ[0], p[1]-tr.PositionXZ[1]))
				if v := cost + d; v < next[mask|bit] {
					next[mask|bit] = v
				}
			}
		}
		costs = next
	}
	best := math.Inf(1)
	for mask, cost := range costs {
		for actor := range truth {
			if mask&(1<<actor) == 0 {
				cost += penalty
			}
		}
		best = math.Min(best, cost)
	}
	return best / float64(len(truth))
}

func fusionResearchKey(d Detection) string {
	return fmt.Sprintf("%s/%s/%d/%d/%d", d.Source, d.SampleID, d.Version.Epoch, d.Version.Sequence, d.MemberIndex)
}

func fusionResearchReplay(tb testing.TB, scene fusionResearchScene, reverse bool) (float64, []Track) {
	tb.Helper()
	tr, err := New(config())
	if err != nil {
		tb.Fatal(err)
	}
	originals := map[string]Detection{}
	var total float64
	var outputs []Track
	for _, frame := range scene.frames {
		inputs := append([]Detection(nil), frame.detections...)
		for _, d := range inputs {
			originals[fusionResearchKey(d)] = d
		}
		if reverse {
			for i, j := 0, len(inputs)-1; i < j; i, j = i+1, j-1 {
				inputs[i], inputs[j] = inputs[j], inputs[i]
			}
		}
		tracks, err := tr.Step(frame.at, inputs)
		if err != nil {
			tb.Fatalf("%s: %v", scene.name, err)
		}
		ids := map[string]bool{}
		for _, track := range tracks {
			if ids[track.ID] || track.ID == "" {
				tb.Fatalf("%s: duplicate/empty track ID", scene.name)
			}
			ids[track.ID] = true
			if track.RunID != config().RunID || track.ClockDomain != config().ClockDomain || track.Frame != config().Frame || track.Schema != Schema || !track.Time.Equal(frame.at) {
				tb.Fatalf("%s: track scope changed", scene.name)
			}
			if !finite(track.PositionXZ[:]...) || !finite(track.VelocityXZ[:]...) || !positive(track.Covariance) {
				tb.Fatalf("%s: invalid estimate", scene.name)
			}
			if len(track.Members) == 0 || !track.ValidUntil.After(frame.at) || track.LastObserved.After(frame.at) || track.ValidUntil.After(track.LastObserved.Add(2*time.Second)) {
				tb.Fatalf("%s: track escaped observation lifetime", scene.name)
			}
			latest := time.Time{}
			sources := map[string]bool{}
			for _, d := range track.Members {
				original, ok := originals[fusionResearchKey(d)]
				if !ok || !reflect.DeepEqual(original, d) || track.ValidUntil.After(d.ValidUntil) || sources[d.Source] {
					tb.Fatalf("%s: original provenance or expiry changed", scene.name)
				}
				sources[d.Source] = true
				if d.SampleTime.After(latest) {
					latest = d.SampleTime
				}
			}
			if !track.LastObserved.Equal(latest) {
				tb.Fatalf("%s: observation timestamp refreshed", scene.name)
			}
		}
		total += fusionResearchError(frame.truth, tracks)
		outputs = append(outputs, tracks...)
	}
	end := scene.frames[len(scene.frames)-1].at.Add(2 * time.Second)
	for _, stale := range [][]Detection{nil, scene.frames[len(scene.frames)-1].detections} {
		tracks, err := tr.Step(end, stale)
		if err != nil || len(tracks) != 0 {
			tb.Fatalf("%s: expired observations survived or resurrected: %v", scene.name, err)
		}
	}
	return total / float64(len(scene.frames)), outputs
}

func TestAutoresearchFusionDeterministicQuality(t *testing.T) {
	for _, scene := range fusionResearchScenes() {
		t.Run(scene.name, func(t *testing.T) {
			score, outputs := fusionResearchReplay(t, scene, false)
			reordered, again := fusionResearchReplay(t, scene, true)
			if score != reordered || !reflect.DeepEqual(outputs, again) {
				t.Fatal("input ordering changed tracking outcomes")
			}
			if math.IsNaN(score) || math.IsInf(score, 0) || score >= 2 {
				t.Fatalf("quality %.6f is no better than returning no tracks", score)
			}
			t.Logf("mean cardinality-penalized position error: %.6f m", score)
		})
	}
}

func TestAutoresearchFusionScoringPenalizesMissingAndExtra(t *testing.T) {
	truth := [][2]float64{{0, 0}, {1, 0}}
	perfect := []Track{{PositionXZ: truth[0]}, {PositionXZ: truth[1]}}
	if fusionResearchError(truth, perfect) != 0 || fusionResearchError(truth, nil) != 2 || fusionResearchError(truth, perfect[:1]) != 1 || fusionResearchError(truth, append(perfect, Track{PositionXZ: truth[0]})) != 1 {
		t.Fatal("scoring failed cardinality penalties")
	}
}

// Every scene reports its own quality objective so acceptance can reject a
// regression in any seed. Timing includes scoring and provenance checks.
func BenchmarkAutoresearchFusion(b *testing.B) {
	for _, scene := range fusionResearchScenes() {
		b.Run(scene.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var score float64
			for i := 0; i < b.N; i++ {
				score, _ = fusionResearchReplay(b, scene, false)
			}
			b.ReportMetric(score, "error-m")
		})
	}
}
