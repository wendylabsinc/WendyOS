package worldview

import (
	"reflect"
	"testing"
)

func TestMatchComposition(t *testing.T) {
	bottle := []Part{
		{Primitive: "cylinder", WidthM: [2]float64{0.06, 0.08}, HeightM: [2]float64{0.18, 0.22}},
		{Primitive: "cylinder", WidthM: [2]float64{0.02, 0.03}, HeightM: [2]float64{0.015, 0.025}},
	}
	body := ObservedPart{Primitive: "rect", WidthM: 0.07, HeightM: 0.2, BottomM: 0}
	capPart := ObservedPart{Primitive: "rect", WidthM: 0.025, HeightM: 0.02, BottomM: 0.2}
	bodyShare, capShare := 0.2/0.22, 0.02/0.22

	lamp := []Part{
		{Primitive: "cylinder", WidthM: [2]float64{0.1, 0.15}, HeightM: [2]float64{0.05, 0.1}},
		{Primitive: "cone", WidthM: [2]float64{0.2, 0.3}, HeightM: [2]float64{0.2, 0.3}},
	}
	base := ObservedPart{Primitive: "rect", WidthM: 0.12, HeightM: 0.07}
	shade := ObservedPart{Primitive: "trapezoid", WidthM: 0.25, HeightM: 0.25}

	cases := []struct {
		name     string
		observed []ObservedPart
		parts    []Part
		score    float64
		visible  []int
	}{
		{"whole bottle", []ObservedPart{body, capPart}, bottle, 1, []int{0, 1}},
		{"input order follows BottomM", []ObservedPart{capPart, body}, bottle, 1, []int{0, 1}},
		{"cap occluded", []ObservedPart{body}, bottle, bodyShare, []int{0}},
		{"body occluded", []ObservedPart{capPart}, bottle, capShare, []int{1}},
		{"cap below body is out of order",
			[]ObservedPart{{Primitive: "rect", WidthM: 0.025, HeightM: 0.02, BottomM: 0}, {Primitive: "rect", WidthM: 0.07, HeightM: 0.2, BottomM: 0.02}},
			bottle, capShare, []int{1}},
		{"lamp in order", []ObservedPart{base, withBottom(shade, 0.07)}, lamp, 1, []int{0, 1}},
		{"lamp upside down", []ObservedPart{shade, withBottom(base, 0.25)}, lamp, 0.25 / 0.325, []int{1}},
		{"clutter between parts is ignored",
			[]ObservedPart{body, {Primitive: "trapezoid", WidthM: 0.07, HeightM: 0.05, BottomM: 0.1}, capPart}, bottle, 1, []int{0, 1}},
		{"partial size fit",
			[]ObservedPart{{Primitive: "rect", WidthM: 0.07, HeightM: 0.2475, BottomM: 0}, capPart}, bottle, 0.5*(0.2/0.22) + capShare, []int{0, 1}},
		{"zero observed", nil, bottle, 0, nil},
		{"no parts", []ObservedPart{body}, nil, 0, nil},
		{"incompatible silhouette", []ObservedPart{{Primitive: "trapezoid", WidthM: 0.07, HeightM: 0.2}}, bottle, 0, nil},
		{"disc silhouette of a cylinder", []ObservedPart{{Primitive: "disc", WidthM: 0.07, HeightM: 0.2}}, bottle, bodyShare, []int{0}},
		{"other never matches", []ObservedPart{{Primitive: "other", WidthM: 0.07, HeightM: 0.2}}, bottle, 0, nil},
		{"solid name is not a silhouette", []ObservedPart{{Primitive: "cylinder", WidthM: 0.07, HeightM: 0.2}}, bottle, 0, nil},
		{"box does not cast a disc", []ObservedPart{{Primitive: "disc", WidthM: 1, HeightM: 1}}, []Part{{Primitive: "box"}}, 0, nil},
		{"unknown solid never matches", []ObservedPart{{Primitive: "rect", WidthM: 1, HeightM: 1}}, []Part{{Primitive: "pyramid"}}, 0, nil},
		{"empty part primitive accepts any silhouette", []ObservedPart{{Primitive: "other", WidthM: 1, HeightM: 1}}, []Part{{Primitive: ""}}, 1, []int{0}},
		{"size fit zero does not match", []ObservedPart{{Primitive: "rect", WidthM: 1, HeightM: 1}}, bottle, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			score, visible := MatchComposition(c.observed, c.parts)
			if !near(score, c.score) || !reflect.DeepEqual(visible, c.visible) {
				t.Fatalf("got %v %v, want %v %v", score, visible, c.score, c.visible)
			}
		})
	}
}

func TestMatchCompositionOccludedCap(t *testing.T) {
	bottle := []Part{
		{Primitive: "cylinder", WidthM: [2]float64{0.06, 0.08}, HeightM: [2]float64{0.18, 0.22}},
		{Primitive: "cylinder", WidthM: [2]float64{0.02, 0.03}, HeightM: [2]float64{0.015, 0.025}},
	}
	capShare := 0.02 / (0.2 + 0.02)
	if capShare >= 0.1 {
		t.Fatalf("fixture: cap share %v is not under 10 percent", capShare)
	}
	score, visible := MatchComposition([]ObservedPart{{Primitive: "rect", WidthM: 0.07, HeightM: 0.2}}, bottle)
	if score <= 0.8 || !reflect.DeepEqual(visible, []int{0}) {
		t.Fatalf("bottle with occluded cap scored %v visible %v, want > 0.8 and [0]", score, visible)
	}
}

func TestMatchCompositionDoesNotMutateInput(t *testing.T) {
	observed := []ObservedPart{{Primitive: "trapezoid", BottomM: 1}, {Primitive: "rect", BottomM: 0}}
	MatchComposition(observed, []Part{{Primitive: "box"}, {Primitive: "cone"}})
	if observed[0].Primitive != "trapezoid" {
		t.Fatalf("input slice was reordered: %+v", observed)
	}
}

func withBottom(p ObservedPart, bottom float64) ObservedPart {
	p.BottomM = bottom
	return p
}
