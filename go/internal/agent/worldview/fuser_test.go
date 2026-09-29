package worldview

import (
	"math"
	"reflect"
	"testing"
)

func avail(attr string, score float64) Evidence {
	return Evidence{Attribute: attr, Score: score, Available: true}
}

func missing(attr string) Evidence { return Evidence{Attribute: attr} }

func TestFuseRenormalisesOverAvailable(t *testing.T) {
	evidence := []Evidence{avail("shape", 0.8), avail("colour", 0.6), missing("size")}
	three := Fuse(evidence, FusionSpec{Weights: map[string]float64{"shape": 0.4, "colour": 0.4, "size": 0.2}})
	two := Fuse([]Evidence{avail("shape", 0.8), avail("colour", 0.6)}, FusionSpec{Weights: map[string]float64{"shape": 0.5, "colour": 0.5}})
	if !near(three.Confidence, two.Confidence) || !near(three.Confidence, 0.7) {
		t.Fatalf("0.4/0.4/0.2 with one unavailable = %v, 0.5/0.5 over two = %v, want both 0.7", three.Confidence, two.Confidence)
	}
	if !reflect.DeepEqual(three.Unavailable, []string{"size"}) {
		t.Fatalf("unavailable = %v", three.Unavailable)
	}
	if _, ok := three.Scores["size"]; ok {
		t.Fatalf("unavailable attribute present in Scores: %v", three.Scores)
	}
}

func TestFuseUnavailableIsNotZero(t *testing.T) {
	weights := map[string]float64{"shape": 1, "size": 1}
	without := Fuse([]Evidence{avail("shape", 0.9)}, FusionSpec{Threshold: 0.8, Weights: weights})
	unavailable := Fuse([]Evidence{avail("shape", 0.9), missing("size")}, FusionSpec{Threshold: 0.8, Weights: weights})
	zero := Fuse([]Evidence{avail("shape", 0.9), avail("size", 0)}, FusionSpec{Threshold: 0.8, Weights: weights})
	if !near(unavailable.Confidence, without.Confidence) || !unavailable.Matched {
		t.Fatalf("unavailable optional changed confidence: %v vs %v (matched %v)", unavailable.Confidence, without.Confidence, unavailable.Matched)
	}
	if !near(zero.Confidence, 0.45) || zero.Matched {
		t.Fatalf("a real zero must lower confidence: got %v matched %v", zero.Confidence, zero.Matched)
	}
	required := Fuse([]Evidence{avail("shape", 0.9), missing("size")}, FusionSpec{Threshold: 0.8, Weights: weights, Required: []string{"size"}})
	if required.Vetoed != "size" || required.Matched {
		t.Fatalf("unavailable required attribute must veto: %+v", required)
	}
	if !near(required.Confidence, 0.9) {
		t.Fatalf("veto should not change the reported confidence: %v", required.Confidence)
	}
}

func TestFuse(t *testing.T) {
	cases := []struct {
		name        string
		evidence    []Evidence
		spec        FusionSpec
		confidence  float64
		unavailable []string
		vetoed      string
		matched     bool
	}{
		{
			name:        "no available attributes",
			evidence:    []Evidence{missing("shape"), missing("colour")},
			spec:        FusionSpec{Threshold: 0},
			unavailable: []string{"colour", "shape"},
		},
		{
			name:        "no evidence at all",
			spec:        FusionSpec{Threshold: 0},
			unavailable: []string{},
		},
		{
			name:        "required absent from evidence vetoes",
			evidence:    []Evidence{avail("shape", 1)},
			spec:        FusionSpec{Threshold: 0.5, Required: []string{"class"}},
			confidence:  1,
			unavailable: []string{"class"},
			vetoed:      "class",
		},
		{
			name:        "unavailable required vetoes even with a zero floor",
			evidence:    []Evidence{avail("shape", 1), missing("size")},
			spec:        FusionSpec{Threshold: 0.5, Required: []string{"size"}, Min: map[string]float64{"size": 0}},
			confidence:  1,
			unavailable: []string{"size"},
			vetoed:      "size",
		},
		{
			name:        "required below default floor vetoes",
			evidence:    []Evidence{avail("shape", 1), avail("size", 0.4)},
			spec:        FusionSpec{Threshold: 0.5, Required: []string{"size"}},
			confidence:  0.7,
			unavailable: []string{},
			vetoed:      "size",
		},
		{
			name:        "required above custom floor",
			evidence:    []Evidence{avail("shape", 1), avail("size", 0.4)},
			spec:        FusionSpec{Threshold: 0.5, Required: []string{"size"}, Min: map[string]float64{"size": 0.3}},
			confidence:  0.7,
			unavailable: []string{},
			matched:     true,
		},
		{
			name:        "first failing required in sorted order",
			evidence:    []Evidence{avail("size", 0.1), avail("colour", 0.1)},
			spec:        FusionSpec{Required: []string{"size", "colour"}},
			confidence:  0.1,
			unavailable: []string{},
			vetoed:      "colour",
		},
		{
			name:        "confidence equal to threshold matches",
			evidence:    []Evidence{avail("shape", 0.5), avail("colour", 1)},
			spec:        FusionSpec{Threshold: 0.75},
			confidence:  0.75,
			unavailable: []string{},
			matched:     true,
		},
		{
			name:        "below threshold does not match",
			evidence:    []Evidence{avail("shape", 0.5)},
			spec:        FusionSpec{Threshold: 0.75},
			confidence:  0.5,
			unavailable: []string{},
		},
		{
			name:        "weights apply",
			evidence:    []Evidence{avail("shape", 1), avail("colour", 0)},
			spec:        FusionSpec{Weights: map[string]float64{"shape": 3, "colour": 1}},
			confidence:  0.75,
			unavailable: []string{},
			matched:     true,
		},
		{
			name:        "missing weight counts as one",
			evidence:    []Evidence{avail("shape", 1), avail("colour", 0)},
			spec:        FusionSpec{Weights: map[string]float64{"shape": 1}},
			confidence:  0.5,
			unavailable: []string{},
			matched:     true,
		},
		{
			name:        "scores are clamped",
			evidence:    []Evidence{avail("shape", 7), avail("colour", math.NaN())},
			spec:        FusionSpec{},
			confidence:  0.5,
			unavailable: []string{},
			matched:     true,
		},
		{
			name:        "first available duplicate wins",
			evidence:    []Evidence{missing("shape"), avail("shape", 0.2), avail("shape", 0.9)},
			spec:        FusionSpec{},
			confidence:  0.2,
			unavailable: []string{},
			matched:     true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Fuse(c.evidence, c.spec)
			if !near(got.Confidence, c.confidence) || got.Vetoed != c.vetoed || got.Matched != c.matched {
				t.Fatalf("got %+v, want confidence %v vetoed %q matched %v", got, c.confidence, c.vetoed, c.matched)
			}
			if !reflect.DeepEqual(got.Unavailable, c.unavailable) {
				t.Fatalf("unavailable = %#v, want %#v", got.Unavailable, c.unavailable)
			}
		})
	}
}

func TestFuseDeterministic(t *testing.T) {
	evidence := []Evidence{avail("a", 0.1), avail("b", 0.2), avail("c", 0.3), avail("d", 0.4), missing("e"), missing("f"), missing("g")}
	spec := FusionSpec{Weights: map[string]float64{"a": 0.1, "b": 0.7, "c": 0.13, "d": 0.29}}
	first := Fuse(evidence, spec)
	for i := 0; i < 200; i++ {
		if got := Fuse(evidence, spec); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differs: %+v vs %+v", i, got, first)
		}
	}
}
