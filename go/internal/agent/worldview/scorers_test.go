package worldview

import (
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

const eps = 1e-9

func near(a, b float64) bool { return math.Abs(a-b) <= eps }

func TestIoU(t *testing.T) {
	cases := []struct {
		name string
		a, b [4]float64
		want float64
	}{
		{"identical", [4]float64{10, 20, 30, 40}, [4]float64{10, 20, 30, 40}, 1},
		{"disjoint", [4]float64{0, 0, 10, 10}, [4]float64{20, 20, 10, 10}, 0},
		{"touching edges", [4]float64{0, 0, 10, 10}, [4]float64{10, 0, 10, 10}, 0},
		{"half overlap", [4]float64{0, 0, 10, 10}, [4]float64{5, 0, 10, 10}, 50.0 / 150.0},
		{"contained", [4]float64{0, 0, 10, 10}, [4]float64{0, 0, 5, 5}, 0.25},
		{"zero width", [4]float64{0, 0, 0, 10}, [4]float64{0, 0, 0, 10}, 0},
		{"negative height", [4]float64{0, 0, 10, -10}, [4]float64{0, 0, 10, 10}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IoU(c.a, c.b); !near(got, c.want) {
				t.Fatalf("IoU = %v, want %v", got, c.want)
			}
			if got := IoU(c.b, c.a); !near(got, c.want) {
				t.Fatalf("IoU reversed = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDeltaE76(t *testing.T) {
	cases := []struct {
		name string
		a, b [3]float64
		want float64
	}{
		{"identical", [3]float64{53.2, 80.1, 67.2}, [3]float64{53.2, 80.1, 67.2}, 0},
		{"lightness only", [3]float64{50, 0, 0}, [3]float64{60, 0, 0}, 10},
		{"three four five", [3]float64{50, 3, 0}, [3]float64{50, 0, 4}, 5},
		{"black to white", [3]float64{0, 0, 0}, [3]float64{100, 0, 0}, 100},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DeltaE76(c.a, c.b); !near(got, c.want) {
				t.Fatalf("DeltaE76 = %v, want %v", got, c.want)
			}
		})
	}
}

func aspect(lo, hi float64) *[2]float64 { return &[2]float64{lo, hi} }

func TestSilhouettesFor(t *testing.T) {
	cases := map[string][]string{
		"cylinder": {"rect", "disc"},
		"box":      {"rect"},
		"sphere":   {"disc"},
		"cone":     {"trapezoid", "disc"},
		"pyramid":  nil,
		"rect":     nil,
		"":         nil,
	}
	for solid, want := range cases {
		if got := SilhouettesFor(solid); !reflect.DeepEqual(got, want) {
			t.Fatalf("SilhouettesFor(%q) = %#v, want %#v", solid, got, want)
		}
	}
	got := SilhouettesFor("cylinder")
	got[0] = "mutated"
	if SilhouettesFor("cylinder")[0] != "rect" {
		t.Fatalf("SilhouettesFor exposes its table")
	}
}

func TestScoreShape(t *testing.T) {
	cases := []struct {
		name      string
		sil       Silhouette
		expect    ShapeExpect
		available bool
		want      float64
	}{
		{"empty primitive is unavailable", Silhouette{"", 2.5}, ShapeExpect{"cylinder", aspect(2, 3)}, false, 0},
		{"cylinder versus rect", Silhouette{"rect", 2.5}, ShapeExpect{"cylinder", nil}, true, 1},
		{"cylinder versus disc", Silhouette{"disc", 1}, ShapeExpect{"cylinder", nil}, true, 1},
		{"cylinder versus trapezoid", Silhouette{"trapezoid", 1}, ShapeExpect{"cylinder", nil}, true, 0},
		{"box versus rect", Silhouette{"rect", 1}, ShapeExpect{"box", nil}, true, 1},
		{"box versus disc", Silhouette{"disc", 1}, ShapeExpect{"box", nil}, true, 0},
		{"sphere versus disc", Silhouette{"disc", 1}, ShapeExpect{"sphere", nil}, true, 1},
		{"sphere versus other", Silhouette{"other", 1}, ShapeExpect{"sphere", nil}, true, 0.25},
		{"cone versus trapezoid", Silhouette{"trapezoid", 1}, ShapeExpect{"cone", nil}, true, 1},
		{"cone versus disc", Silhouette{"disc", 1}, ShapeExpect{"cone", nil}, true, 1},
		{"cone versus rect", Silhouette{"rect", 1}, ShapeExpect{"cone", nil}, true, 0},
		{"unlisted silhouette counts as other", Silhouette{"blob", 1}, ShapeExpect{"cone", nil}, true, 0.25},
		{"solid name as silhouette counts as other", Silhouette{"cylinder", 1}, ShapeExpect{"cylinder", nil}, true, 0.25},
		{"compatible inside range", Silhouette{"rect", 2.5}, ShapeExpect{"cylinder", aspect(2, 3)}, true, 1},
		{"compatible on bound", Silhouette{"rect", 3}, ShapeExpect{"cylinder", aspect(2, 3)}, true, 1},
		{"quarter width above", Silhouette{"rect", 3.25}, ShapeExpect{"cylinder", aspect(2, 3)}, true, 0.5},
		{"quarter width below", Silhouette{"rect", 1.75}, ShapeExpect{"cylinder", aspect(2, 3)}, true, 0.5},
		{"half width above is zero", Silhouette{"rect", 3.5}, ShapeExpect{"cylinder", aspect(2, 3)}, true, 0},
		{"far above is zero", Silhouette{"rect", 9}, ShapeExpect{"cylinder", aspect(2, 3)}, true, 0},
		{"reversed range", Silhouette{"rect", 3.25}, ShapeExpect{"cylinder", aspect(3, 2)}, true, 0.5},
		{"point range uses value", Silhouette{"rect", 2.5}, ShapeExpect{"box", aspect(2, 2)}, true, 0.5},
		{"point range exact", Silhouette{"rect", 2}, ShapeExpect{"box", aspect(2, 2)}, true, 1},
		{"other times aspect", Silhouette{"other", 3.25}, ShapeExpect{"cylinder", aspect(2, 3)}, true, 0.125},
		{"incompatible ignores aspect", Silhouette{"disc", 2.5}, ShapeExpect{"box", aspect(2, 3)}, true, 0},
		{"no aspect expected", Silhouette{"rect", 40}, ShapeExpect{"box", nil}, true, 1},
		{"no primitive expected", Silhouette{"trapezoid", 2.5}, ShapeExpect{"", aspect(2, 3)}, true, 1},
		{"NaN aspect", Silhouette{"rect", math.NaN()}, ShapeExpect{"cylinder", aspect(2, 3)}, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := ScoreShape(Proposal{Silhouette: c.sil}, c.expect)
			if ev.Attribute != AttributeShape {
				t.Fatalf("attribute = %q", ev.Attribute)
			}
			if ev.Available != c.available || !near(ev.Score, c.want) {
				t.Fatalf("got available=%v score=%v, want available=%v score=%v", ev.Available, ev.Score, c.available, c.want)
			}
		})
	}
}

func TestScoreShapeUnknownSolid(t *testing.T) {
	ev := ScoreShape(Proposal{Silhouette: Silhouette{"rect", 2}}, ShapeExpect{Primitive: "pyramid"})
	if ev.Available {
		t.Fatalf("unknown expected primitive is available: %+v", ev)
	}
	reason, _ := ev.Detail["reason"].(string)
	if !strings.Contains(reason, `"pyramid"`) || !strings.Contains(reason, "not a known solid") {
		t.Fatalf("reason = %q", reason)
	}
}

func TestScoreSize(t *testing.T) {
	bottle := SizeExpect{WidthM: [2]float64{0.06, 0.08}, HeightM: [2]float64{0.18, 0.22}}
	heightOnly := SizeExpect{HeightM: [2]float64{1, 2}}
	cases := []struct {
		name      string
		metric    *Metric
		expect    SizeExpect
		available bool
		want      float64
	}{
		{"no depth is unavailable", nil, bottle, false, 0},
		{"inside", &Metric{WidthM: 0.07, HeightM: 0.2}, bottle, true, 1},
		{"height halfway through falloff", &Metric{WidthM: 0.07, HeightM: 0.2475}, bottle, true, 0.5},
		{"width at 25 percent below is zero", &Metric{WidthM: 0.045, HeightM: 0.2}, bottle, true, 0},
		{"minimum of dimensions", &Metric{WidthM: 0.09, HeightM: 0.2475}, bottle, true, 0.5},
		{"width falloff above", &Metric{WidthM: 0.09, HeightM: 0.2}, bottle, true, 0.5},
		{"distance is never scored", &Metric{WidthM: 0.07, HeightM: 0.2, DistanceM: 1e6}, bottle, true, 1},
		{"unconstrained width ignored", &Metric{WidthM: 50, HeightM: 1.5}, heightOnly, true, 1},
		{"nothing constrained is unavailable", &Metric{WidthM: 1, HeightM: 1}, SizeExpect{}, false, 0},
		{"infinite height", &Metric{WidthM: 0.07, HeightM: math.Inf(1)}, bottle, true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := ScoreSize(Proposal{Metric: c.metric}, c.expect)
			if ev.Attribute != AttributeSize {
				t.Fatalf("attribute = %q", ev.Attribute)
			}
			if ev.Available != c.available || !near(ev.Score, c.want) {
				t.Fatalf("got available=%v score=%v, want available=%v score=%v", ev.Available, ev.Score, c.available, c.want)
			}
		})
	}
}

func TestScoreColour(t *testing.T) {
	grey := [3]float64{50, 0, 0}
	red := [3]float64{50, 60, 0}
	expect := ColourExpect{Palette: []PaletteEntry{{grey, 0.6}, {red, 0.4}}, Tolerance: 20}
	cases := []struct {
		name      string
		palette   []PaletteEntry
		expect    ColourExpect
		available bool
		want      float64
	}{
		{"empty palette is unavailable", nil, expect, false, 0},
		{"no expected palette is unavailable", []PaletteEntry{{grey, 1}}, ColourExpect{}, false, 0},
		{"identical", []PaletteEntry{{grey, 0.6}, {red, 0.4}}, expect, true, 1},
		{"order of observed entries is irrelevant", []PaletteEntry{{red, 0.4}, {grey, 0.6}}, expect, true, 1},
		{"red off by half tolerance", []PaletteEntry{{grey, 0.6}, {[3]float64{50, 70, 0}, 0.4}}, expect, true, 0.6 + 0.4*0.5},
		{"grey at half its share", []PaletteEntry{{grey, 0.3}, {red, 0.4}}, expect, true, 0.6*0.5 + 0.4},
		{"surplus share is capped", []PaletteEntry{{grey, 0.9}, {red, 0.9}}, expect, true, 1},
		{"far colour", []PaletteEntry{{[3]float64{0, 0, 0}, 1}}, expect, true, 0},
		{"zero tolerance uses default of 25", []PaletteEntry{{grey, 0.6}, {[3]float64{50, 70, 0}, 0.4}},
			ColourExpect{Palette: expect.Palette}, true, 0.6 + 0.4*(1-10.0/25)},
		{"zero expected shares weigh equally", []PaletteEntry{{grey, 0.5}, {[3]float64{50, 70, 0}, 0.5}},
			ColourExpect{Palette: []PaletteEntry{{grey, 0}, {red, 0}}, Tolerance: 20}, true, (1 + 0.5) / 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := ScoreColour(Proposal{Palette: c.palette}, c.expect)
			if ev.Attribute != AttributeColour {
				t.Fatalf("attribute = %q", ev.Attribute)
			}
			if ev.Available != c.available || !near(ev.Score, c.want) {
				t.Fatalf("got available=%v score=%v, want available=%v score=%v", ev.Available, ev.Score, c.available, c.want)
			}
		})
	}
}

func TestScoreClass(t *testing.T) {
	const at = int64(10 * time.Second)
	p := Proposal{SourceID: "cam0", BootNanos: at, Box: [4]float64{0, 0, 100, 100}}
	same := [4]float64{0, 0, 100, 100}
	shifted := [4]float64{60, 0, 100, 100}  // IoU 0.25
	partial := [4]float64{10, 10, 100, 100} // IoU 8100 / 11900
	bottle := ClassExpect{Label: "bottle"}
	cases := []struct {
		name      string
		expect    ClassExpect
		evidence  []ClassEvidence
		window    time.Duration
		available bool
		want      float64
	}{
		{"no evidence is unavailable", bottle, nil, 0, false, 0},
		{"other source is unavailable", bottle, []ClassEvidence{{SourceID: "cam1", Label: "bottle", Confidence: 0.9, Box: same, BootNanos: at}}, 0, false, 0},
		{"outside default window is unavailable", bottle, []ClassEvidence{{SourceID: "cam0", Label: "bottle", Confidence: 0.9, Box: same, BootNanos: at + int64(600*time.Millisecond)}}, 0, false, 0},
		{"window edge is inside", bottle, []ClassEvidence{{SourceID: "cam0", Label: "bottle", Confidence: 0.9, Box: same, BootNanos: at - int64(500*time.Millisecond)}}, 0, true, 0.9},
		{"wider window passed in", bottle, []ClassEvidence{{SourceID: "cam0", Label: "bottle", Confidence: 0.9, Box: same, BootNanos: at + int64(600*time.Millisecond)}}, time.Second, true, 0.9},
		{"match yields confidence", bottle, []ClassEvidence{{SourceID: "cam0", Label: "bottle", Confidence: 0.9, Box: same, BootNanos: at}}, 0, true, 0.9},
		{"low overlap scores zero", bottle, []ClassEvidence{{SourceID: "cam0", Label: "bottle", Confidence: 0.9, Box: shifted, BootNanos: at}}, 0, true, 0},
		{"other label scores zero", bottle, []ClassEvidence{{SourceID: "cam0", Label: "cup", Confidence: 0.9, Box: same, BootNanos: at}}, 0, true, 0},
		{"model mismatch scores zero", ClassExpect{Label: "bottle", Model: "detr"}, []ClassEvidence{{SourceID: "cam0", Label: "bottle", Model: "yolo", Confidence: 0.9, Box: same, BootNanos: at}}, 0, true, 0},
		{"model match", ClassExpect{Label: "bottle", Model: "detr"}, []ClassEvidence{{SourceID: "cam0", Label: "bottle", Model: "detr", Confidence: 0.7, Box: same, BootNanos: at}}, 0, true, 0.7},
		{"best IoU wins over higher confidence", bottle, []ClassEvidence{
			{SourceID: "cam0", Label: "bottle", Confidence: 0.95, Box: partial, BootNanos: at},
			{SourceID: "cam0", Label: "bottle", Confidence: 0.6, Box: same, BootNanos: at},
		}, 0, true, 0.6},
		{"IoU tie goes to higher confidence", bottle, []ClassEvidence{
			{SourceID: "cam0", Label: "bottle", Confidence: 0.6, Box: same, BootNanos: at},
			{SourceID: "cam0", Label: "bottle", Confidence: 0.8, Box: same, BootNanos: at},
		}, 0, true, 0.8},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev := ScoreClass(p, c.expect, c.evidence, c.window)
			if ev.Attribute != AttributeClass {
				t.Fatalf("attribute = %q", ev.Attribute)
			}
			if ev.Available != c.available || !near(ev.Score, c.want) {
				t.Fatalf("got available=%v score=%v, want available=%v score=%v", ev.Available, ev.Score, c.available, c.want)
			}
		})
	}
}
