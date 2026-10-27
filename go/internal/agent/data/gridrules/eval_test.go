package gridrules

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
)

// testGrid is a w by h grid of the given values, stored the way a producer
// stores them: counts with a scale and an offset, here tenths of a unit from
// -100, and lines padded past the cells, so a rule that ignored the grid's own
// scale, offset or line length would read wrong values.
func testGrid(w, h int, value func(col, row int) float64) *datapb.Grid {
	const scale, offset = 0.1, -100.0
	bpl := 2*w + 6
	data := make([]byte, bpl*h)
	for row := 0; row < h; row++ {
		for col := 0; col < w; col++ {
			count := math.Round((value(col, row) - offset) / scale)
			binary.LittleEndian.PutUint16(data[row*bpl+2*col:], uint16(count))
		}
	}
	return &datapb.Grid{Width: uint32(w), Height: uint32(h), Unit: "°C", Data: data, BytesPerLine: uint32(bpl), Scale: scale, Offset: offset, Camera: "/dev/video2"}
}

// scene is a background value with rectangles of other values.
type rect struct {
	c0, r0, c1, r1 int // inclusive
	v              float64
}

func scene(background float64, rects ...rect) func(int, int) float64 {
	return func(col, row int) float64 {
		v := background
		for _, r := range rects {
			if col >= r.c0 && col <= r.c1 && row >= r.r0 && row <= r.r1 {
				v = r.v
			}
		}
		return v
	}
}

func cells(points ...[2]int) func(int, int) float64 {
	return func(col, row int) float64 {
		for _, p := range points {
			if p[0] == col && p[1] == row {
				return 60
			}
		}
		return 20
	}
}

func program(t *testing.T, steps, outputs string) *Program {
	t.Helper()
	return mustCompile(t, "name: r\ninput: agent/camera/grid/x\nsteps:\n"+steps+"outputs:\n"+outputs)
}

const twoBands = "  - bands: [{name: warm, min: 40, max: 50, color: yellow}, {name: hot, min: 50, color: red}]\n"

func evaluate(t *testing.T, e *Evaluator, g *datapb.Grid, at time.Duration) Frame {
	t.Helper()
	f, err := e.Evaluate(g, int64(at))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// A band holds its min and not its max, and a value is the grid's count times
// its scale plus its offset.
func TestBandsClassifyByTheGridsOwnScale(t *testing.T) {
	p := program(t, twoBands, "  markers: areas\n")
	values := []float64{39.9, 40, 45, 49.9, 50, 63.1, 20}
	g := testGrid(len(values)*2, 1, func(col, _ int) float64 { return values[col/2] })
	f := evaluate(t, NewEvaluator(p), g, 0)
	got := map[string][]float64{}
	for _, a := range f.Areas {
		got[a.Band] = append(got[a.Band], a.Max)
	}
	// 40, 45 and 49.9 touch, so they are one warm area; 50 and 63.1 one hot.
	if len(f.Areas) != 2 || len(got["warm"]) != 1 || len(got["hot"]) != 1 {
		t.Fatalf("areas %+v", f.Areas)
	}
	if math.Abs(got["warm"][0]-49.9) > 1e-9 || math.Abs(got["hot"][0]-63.1) > 1e-9 {
		t.Fatalf("maxima %v", got)
	}
	if f.Cells != 14 || math.Abs(f.RegionMax-63.1) > 1e-9 || math.Abs(f.RegionMin-20) > 1e-9 {
		t.Fatalf("region %d cells, %g..%g", f.Cells, f.RegionMin, f.RegionMax)
	}
}

// Cells of one band that touch only at a corner are one area: a thin hot
// object at an angle stays one area and is not dropped by min_cells as single
// cells. A gap of one cell separates areas.
func TestAreasAreEightConnected(t *testing.T) {
	p := program(t, twoBands+"  - areas: {min_cells: 4}\n", "  markers: areas\n")
	diagonal := cells([2]int{1, 1}, [2]int{2, 2}, [2]int{3, 3}, [2]int{4, 4}, [2]int{5, 5})
	f := evaluate(t, NewEvaluator(p), testGrid(10, 10, diagonal), 0)
	if len(f.Areas) != 1 || f.Areas[0].Cells != 5 || f.Areas[0].Band != "hot" {
		t.Fatalf("diagonal line: %+v", f.Areas)
	}
	p = program(t, twoBands, "  markers: areas\n")
	apart := cells([2]int{1, 1}, [2]int{3, 1}, [2]int{1, 3})
	if f := evaluate(t, NewEvaluator(p), testGrid(10, 10, apart), 0); len(f.Areas) != 3 {
		t.Fatalf("cells a cell apart: %+v", f.Areas)
	}
}

// Touching cells of different bands are different areas.
func TestAreasHoldOneBand(t *testing.T) {
	p := program(t, twoBands, "  markers: areas\n")
	g := testGrid(10, 10, scene(20, rect{2, 2, 7, 7, 45}, rect{4, 4, 5, 5, 70}))
	f := evaluate(t, NewEvaluator(p), g, 0)
	if len(f.Areas) != 2 {
		t.Fatalf("areas %+v", f.Areas)
	}
	hot, warm := f.Areas[0], f.Areas[1]
	if hot.Band != "hot" || hot.Cells != 4 || warm.Band != "warm" || warm.Cells != 32 || hot.Color != "red" || warm.Color != "yellow" {
		t.Fatalf("areas %+v", f.Areas)
	}
}

// An area smaller than min_cells is dropped; one of exactly min_cells stays.
func TestAreasMinCells(t *testing.T) {
	p := program(t, twoBands+"  - areas: {min_cells: 4}\n", "  markers: areas\n")
	g := testGrid(12, 12, scene(20, rect{1, 1, 3, 1, 60}, rect{6, 6, 7, 7, 60}))
	f := evaluate(t, NewEvaluator(p), g, 0)
	if len(f.Areas) != 1 || f.Areas[0].Cells != 4 || f.Areas[0].Left != 6.0/12 {
		t.Fatalf("areas %+v", f.Areas)
	}
}

// An area carries its highest and mean value, the centre of its hottest cell,
// and its bounding box normalised to the grid, as a box marker places it.
func TestAreaStatistics(t *testing.T) {
	p := program(t, twoBands, "  markers: areas\n")
	// Columns 4..7 and rows 2..3 of a 16x8 grid at 55, one cell at 70.
	g := testGrid(16, 8, scene(20, rect{4, 2, 7, 3, 55}, rect{6, 3, 6, 3, 70}))
	a := evaluate(t, NewEvaluator(p), g, 0).Areas[0]
	if a.Cells != 8 || math.Abs(a.Max-70) > 1e-9 || math.Abs(a.Mean-(7*55+70)/8.0) > 1e-9 {
		t.Fatalf("area %+v", a)
	}
	if a.Left != 4.0/16 || a.Right != 8.0/16 || a.Top != 2.0/8 || a.Bottom != 4.0/8 {
		t.Fatalf("edges %+v", a)
	}
	if a.MaxX != 6.5/16 || a.MaxY != 3.5/8 {
		t.Fatalf("hottest cell at %g,%g", a.MaxX, a.MaxY)
	}
	x, y, w, h := a.Box()
	if x != 6.0/16 || y != 3.0/8 || w != 4.0/16 || h != 2.0/8 {
		t.Fatalf("box %g %g %g %g", x, y, w, h)
	}
}

// The region keeps cells whose centre lies in it. A hot spot outside is
// ignored, and an area that crosses the edge is cut at it.
func TestRegion(t *testing.T) {
	p := program(t, "  - region: {x: 0, y: 0, width: 0.5, height: 1}\n"+twoBands, "  markers: areas\n")
	g := testGrid(10, 4, scene(20, rect{3, 0, 6, 1, 60}, rect{8, 3, 9, 3, 90}))
	f := evaluate(t, NewEvaluator(p), g, 0)
	if f.Cells != 20 || math.Abs(f.RegionMax-60) > 1e-9 {
		t.Fatalf("region %d cells, max %g", f.Cells, f.RegionMax)
	}
	if len(f.Areas) != 1 || f.Areas[0].Cells != 4 || f.Areas[0].Right != 0.5 {
		t.Fatalf("areas %+v", f.Areas)
	}
	// A region narrower than a cell, between two centres, holds no cell.
	p = program(t, "  - region: {x: 0.01, y: 0, width: 0.02, height: 1}\n"+twoBands, "  markers: areas\n")
	if f := evaluate(t, NewEvaluator(p), g, 0); f.Cells != 0 || len(f.Areas) != 0 {
		t.Fatalf("thin region: %+v", f)
	}
}

// The same steps on a brightness grid: percent, a different scale, and a band
// with only a max.
func TestStepsAreGeneric(t *testing.T) {
	p := program(t, "  - bands: [{name: dark, max: 10, color: blue}]\n  - areas: {min_cells: 2}\n",
		"  events:\n    - {name: lens_covered, when: area.cells >= 50}\n")
	data := make([]byte, 2*10*10)
	for i := 0; i < 100; i++ {
		v := uint16(52428) // 80 %
		if i < 60 {
			v = 3277 // 5 %
		}
		binary.LittleEndian.PutUint16(data[2*i:], v)
	}
	g := &datapb.Grid{Width: 10, Height: 10, Unit: "%", Data: data, BytesPerLine: 20, Scale: 100.0 / 65535}
	f := evaluate(t, NewEvaluator(p), g, 0)
	if len(f.Areas) != 1 || f.Areas[0].Band != "dark" || f.Areas[0].Cells != 60 || len(f.Fired) != 1 || f.Fired[0].Event != "lens_covered" {
		t.Fatalf("frame %+v", f)
	}
}

// eventRun feeds frames and reports the frame times at which an event fired.
type eventRun struct {
	t *testing.T
	e *Evaluator
}

func (r eventRun) frame(at time.Duration, g *datapb.Grid) []string {
	r.t.Helper()
	var names []string
	for _, f := range evaluate(r.t, r.e, g, at).Fired {
		if f.FrameNanos != int64(at) {
			r.t.Fatalf("firing at %d for a frame at %d", f.FrameNanos, at)
		}
		names = append(names, f.Event)
	}
	return names
}

// frames feeds frames every step from start to end inclusive, and returns
// when each fired.
func (r eventRun) frames(start, end, step time.Duration, g *datapb.Grid) []time.Duration {
	r.t.Helper()
	var fired []time.Duration
	for at := start; at <= end; at += step {
		if len(r.frame(at, g)) > 0 {
			fired = append(fired, at)
		}
	}
	return fired
}

var (
	cold    = testGrid(8, 8, scene(20))
	warm    = testGrid(8, 8, scene(20, rect{2, 2, 4, 4, 45}))
	hot     = testGrid(8, 8, scene(20, rect{2, 2, 4, 4, 63.1}))
	cooling = testGrid(8, 8, scene(20, rect{2, 2, 4, 4, 49})) // below hot, above clear_below
	cooled  = testGrid(8, 8, scene(20, rect{2, 2, 4, 4, 47}))
)

func alarmRun(t *testing.T, event string) eventRun {
	t.Helper()
	return eventRun{t, NewEvaluator(program(t, twoBands+"  - areas: {min_cells: 4}\n", "  events:\n    - "+event+"\n"))}
}

// for counts frame time: the condition must hold from the first frame it held
// on for the whole duration, and one frame without it starts the count over.
// Frames are evaluated back to back, so only their timestamps can pass time.
func TestForCountsFrameTime(t *testing.T) {
	r := alarmRun(t, "{name: alarm, when: area.band == hot, for: 2s}")
	ms := time.Millisecond
	if fired := r.frames(0, 1960*ms, 40*ms, hot); len(fired) != 0 {
		t.Fatalf("fired at %v, before 2s", fired)
	}
	if got := r.frame(2000*ms, hot); len(got) != 1 || got[0] != "alarm" {
		t.Fatalf("at 2s: %v", got)
	}

	r = alarmRun(t, "{name: alarm, when: area.band == hot, for: 2s}")
	r.frames(0, 1500*ms, 100*ms, hot)
	r.frame(1600*ms, cold)
	if fired := r.frames(1700*ms, 3600*ms, 100*ms, hot); len(fired) != 0 {
		t.Fatalf("a cold frame did not restart the count: fired at %v", fired)
	}
	if fired := r.frames(3700*ms, 3700*ms, 100*ms, hot); len(fired) != 1 {
		t.Fatal("no event 2s after the condition held again")
	}

	// Irregular frames: what counts is the time between first and last.
	r = alarmRun(t, "{name: alarm, when: area.band == hot, for: 1s}")
	for _, at := range []time.Duration{10 * ms, 23 * ms, 400 * ms, 401 * ms, 1009 * ms} {
		if len(r.frame(at, hot)) != 0 {
			t.Fatalf("fired at %v", at)
		}
	}
	if len(r.frame(1010*ms, hot)) != 1 {
		t.Fatal("no event at 1s of frame time")
	}
}

// With clear_below, a raised event stays raised until every value in the
// region is below it, so a temperature that hovers around the band's edge
// raises one event, not one per crossing.
func TestClearBelowHysteresis(t *testing.T) {
	r := alarmRun(t, "{name: alarm, when: area.band == hot, clear_below: 48}")
	s := time.Second
	steps := []struct {
		g    *datapb.Grid
		want int
	}{
		{hot, 1}, {cooling, 0}, {hot, 0}, {cooling, 0}, {hot, 0}, // hovering: still raised
		{cooled, 0}, // below 48: cleared
		{hot, 1},
	}
	for i, step := range steps {
		if got := r.frame(time.Duration(i)*2*s, step.g); len(got) != step.want {
			t.Fatalf("frame %d: %d events, want %d", i, len(got), step.want)
		}
	}
}

// Without hysteresis, an event clears on the first frame its condition does
// not hold, and raises again when it holds again.
func TestClearsWithoutHysteresis(t *testing.T) {
	r := alarmRun(t, "{name: alarm, when: area.band == hot}")
	s := time.Second
	for i, step := range []struct {
		g    *datapb.Grid
		want int
	}{{hot, 1}, {hot, 0}, {cooling, 0}, {hot, 1}, {warm, 0}, {hot, 1}} {
		if got := r.frame(time.Duration(i)*2*s, step.g); len(got) != step.want {
			t.Fatalf("frame %d: %d events, want %d", i, len(got), step.want)
		}
	}
}

// cooldown is the least time between two events, in frame time. A condition
// that holds when the cooldown ends fires then.
func TestCooldown(t *testing.T) {
	r := alarmRun(t, "{name: alarm, when: area.band == hot, cooldown: 2m}")
	s := time.Second
	if len(r.frame(0, hot)) != 1 {
		t.Fatal("no first event")
	}
	r.frame(10*s, cold)
	if fired := r.frames(11*s, 119*s, s, hot); len(fired) != 0 {
		t.Fatalf("fired during the cooldown at %v", fired)
	}
	if len(r.frame(120*s, hot)) != 1 {
		t.Fatal("no event when the cooldown ended")
	}
}

// Whatever its cooldown, an event fires at most once a second, so a condition
// that flickers at frame rate cannot flood the journal.
func TestMinEventSpacing(t *testing.T) {
	r := alarmRun(t, "{name: alarm, when: area.band == hot}")
	ms := time.Millisecond
	fired := 0
	for i := 0; i < 250; i++ { // 10 s at 25 Hz
		g := cold
		if i%2 == 0 {
			g = hot
		}
		fired += len(r.frame(time.Duration(i)*40*ms, g))
	}
	if fired != 10 {
		t.Fatalf("%d events in 10 s of flicker, want one a second", fired)
	}
}

// A gap of more than MaxFrameGap in the frames starts the timing over: a for
// counted before it does not count, and a raised event raises again after it,
// cooldown permitting.
func TestFrameGapRestartsTiming(t *testing.T) {
	s := time.Second
	r := alarmRun(t, "{name: alarm, when: area.band == hot, for: 2s}")
	r.frames(0, 1500*time.Millisecond, 100*time.Millisecond, hot)
	if len(r.frame(7*s, hot)) != 0 {
		t.Fatal("a for counted across a gap")
	}
	if len(r.frame(9*s, hot)) != 1 {
		t.Fatal("no event 2s after the gap")
	}
	if len(r.frame(10*s, hot)) != 0 {
		t.Fatal("raised event fired again")
	}
	r.frame(20*s, hot) // gap: no longer raised, for starts again
	if len(r.frame(22*s, hot)) != 1 {
		t.Fatal("an event still raised across a gap did not raise again")
	}

	r = alarmRun(t, "{name: alarm, when: area.band == hot, cooldown: 1m}")
	r.frame(0, hot)
	if fired := r.frames(10*s, 50*s, 10*s, hot); len(fired) != 0 { // gaps of 10 s
		t.Fatalf("a gap cut the cooldown short: fired at %v", fired)
	}
	if len(r.frame(60*s, hot)) != 1 {
		t.Fatal("no event after the cooldown")
	}
}

// A frame time that steps back starts the timing over, and restarts a
// cooldown from that frame rather than letting it lapse.
func TestBackwardsFrameTime(t *testing.T) {
	s := time.Second
	r := alarmRun(t, "{name: alarm, when: area.band == hot, cooldown: 30s}")
	base := 1000 * s
	r.frame(base, hot)
	if len(r.frame(base-500*s, hot)) != 0 {
		t.Fatal("a step back ended the cooldown")
	}
	if fired := r.frames(base-499*s, base-471*s, s, hot); len(fired) != 0 {
		t.Fatalf("fired at %v", fired)
	}
	if len(r.frame(base-470*s, hot)) != 1 {
		t.Fatal("no event 30 s after the step back")
	}
}

// Two events of one rule run independently, and an event reports the area
// that met its condition with the highest value.
func TestEventsReportTheirArea(t *testing.T) {
	p := program(t, twoBands, "  events:\n    - {name: warning, when: area.band == warm}\n    - {name: alarm, when: area.band == hot}\n    - {name: big, when: area.cells >= 6}\n")
	e := NewEvaluator(p)
	g := testGrid(10, 10, scene(20, rect{0, 0, 1, 2, 45}, rect{5, 5, 5, 5, 58}, rect{7, 7, 8, 8, 66}))
	f := evaluate(t, e, g, 0)
	got := map[string]Area{}
	for _, fired := range f.Fired {
		got[fired.Event] = fired.Area
	}
	if len(got) != 3 || got["warning"].Band != "warm" || got["warning"].Cells != 6 || math.Abs(got["alarm"].Max-66) > 1e-9 || got["big"].Band != "warm" {
		t.Fatalf("fired %+v", f.Fired)
	}
}

// A grid without values is refused and leaves the timing alone.
func TestEvaluateRefusesGridsWithoutValues(t *testing.T) {
	p := program(t, twoBands, "  markers: areas\n")
	good := testGrid(4, 4, scene(20))
	for name, g := range map[string]*datapb.Grid{
		"scale 0":    {Width: 4, Height: 4, Data: good.Data, BytesPerLine: good.BytesPerLine},
		"short data": {Width: 4, Height: 4, Data: good.Data[:20], BytesPerLine: good.BytesPerLine, Scale: 1},
		"short line": {Width: 4, Height: 4, Data: good.Data, BytesPerLine: 6, Scale: 1},
		"empty":      {Scale: 1},
		"huge":       {Width: 1 << 20, Height: 1, Scale: 1},
		"nan":        {Width: 4, Height: 4, Data: good.Data, BytesPerLine: good.BytesPerLine, Scale: math.NaN()},
	} {
		if _, err := NewEvaluator(p).Evaluate(g, 0); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Markers are a box per area in its band's colour, labelled with the band and
// its highest value in the grid's unit, on the grid's camera, with its
// correction. No area clears the boxes.
func TestMarkers(t *testing.T) {
	p := program(t, twoBands, "  markers: areas\n")
	g := testGrid(16, 8, scene(20, rect{4, 2, 7, 3, 52.3}))
	g.Correction = &datapb.Correction{Gain: 0.48}
	m := Markers(evaluate(t, NewEvaluator(p), g, 0), g, 2*time.Second)
	if m.Camera != "/dev/video2" || m.ValidForNanos != int64(2*time.Second) || !proto.Equal(m.Correction, g.Correction) || len(m.Markers) != 1 {
		t.Fatalf("markers %v", m)
	}
	box := m.Markers[0]
	if box.Kind != datapb.Marker_KIND_BOX || box.X != 6.0/16 || box.Y != 3.0/8 || box.Width != 0.25 || box.Height != 0.25 ||
		box.Label != "hot" || box.Color != "red" || box.Unit != "°C" || math.Abs(box.GetValue()-52.3) > 1e-9 {
		t.Fatalf("box %v", box)
	}
	empty := Markers(evaluate(t, NewEvaluator(p), testGrid(16, 8, scene(20)), 0), g, time.Second)
	if len(empty.Markers) != 0 || empty.Camera != "/dev/video2" {
		t.Fatalf("no areas: %v", empty)
	}
	// A frame of many areas draws the MaxMarkers with the highest values.
	speckled := testGrid(40, 40, func(col, row int) float64 {
		if col%2 == 0 && row%2 == 0 {
			return 50 + float64(col*40+row)/100
		}
		return 20
	})
	many := Markers(evaluate(t, NewEvaluator(p), speckled, 0), speckled, time.Second)
	if len(many.Markers) != MaxMarkers || many.Markers[0].GetValue() < many.Markers[MaxMarkers-1].GetValue() {
		t.Fatalf("%d markers", len(many.Markers))
	}
}

// A rule runs on every frame a decoder makes, so it must not allocate per
// frame once its buffers fit the grid.
func TestEvaluateDoesNotAllocate(t *testing.T) {
	p := mustCompile(t, exampleRule)
	e := NewEvaluator(p)
	g := testGrid(256, 192, scene(22, rect{100, 80, 110, 90, 45}, rect{103, 83, 106, 86, 63}, rect{200, 10, 202, 12, 55}))
	at := int64(0)
	evaluate(t, e, g, 0)
	if allocs := testing.AllocsPerRun(20, func() {
		at += int64(40 * time.Millisecond)
		if _, err := e.Evaluate(g, at); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("%v allocations per frame", allocs)
	}
}

func BenchmarkEvaluateThermalGrid(b *testing.B) {
	var rule Rule
	if err := yaml.Unmarshal([]byte(exampleRule), &rule); err != nil {
		b.Fatal(err)
	}
	p, err := rule.Compile()
	if err != nil {
		b.Fatal(err)
	}
	e := NewEvaluator(p)
	g := testGrid(256, 192, scene(22, rect{100, 80, 140, 120, 45}, rect{110, 90, 130, 110, 63}))
	for i := 0; b.Loop(); i++ {
		if _, err := e.Evaluate(g, int64(i)*int64(40*time.Millisecond)); err != nil {
			b.Fatal(err)
		}
	}
}
