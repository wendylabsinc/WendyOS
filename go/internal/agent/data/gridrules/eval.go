package gridrules

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/proto/gen/datapb"
)

// maxGridSide bounds a grid the evaluator accepts, so a malformed grid cannot
// make it allocate without limit.
const maxGridSide = 4096

// MaxMarkers is the most boxes a frame's markers carry: the areas with the
// highest values. Events see every area.
const MaxMarkers = 64

// Area is a set of touching cells of one band, at least the rule's min_cells
// large. Positions are normalised to 0..1 against the grid, which covers its
// camera's picture, in the convention of wendy.data.v1 positions.
type Area struct {
	Band  string
	Color string
	band  int
	Cells int
	// Max and Mean are the area's highest and mean value, in the grid's unit.
	Max, Mean float64
	// MaxX and MaxY are the centre of the first cell holding Max.
	MaxX, MaxY float64
	// Left, Top, Right and Bottom are the edges of the area's bounding box.
	Left, Top, Right, Bottom float64
}

// Box is the area's bounding box as a wendy.data.v1.Marker box: its centre,
// width and height.
func (a Area) Box() (x, y, width, height float64) {
	return (a.Left + a.Right) / 2, (a.Top + a.Bottom) / 2, a.Right - a.Left, a.Bottom - a.Top
}

// Firing is an event a frame raised.
type Firing struct {
	Event string
	// Area is the area that met the event's condition; with several, the one
	// with the highest value.
	Area Area
	// FrameNanos is the time of the frame that raised it, as Evaluate was
	// given it.
	FrameNanos int64
}

// Frame is what one grid frame showed.
type Frame struct {
	// Cells is how many cells the region covers at this grid's size.
	Cells int
	// RegionMax and RegionMin are the highest and lowest value in the region,
	// when Cells is not 0.
	RegionMax, RegionMin float64
	// Areas are the areas of at least min_cells cells, highest Max first. The
	// slice is reused by the next Evaluate.
	Areas []Area
	// Fired are the events this frame raised. Reused by the next Evaluate.
	Fired []Firing
}

// Evaluator runs one rule on the frames of one grid stream, in order. It keeps
// the events' timing between frames, so a stream needs its own evaluator. It
// is not safe for concurrent use.
type Evaluator struct {
	p      *Program
	states []eventState
	// Scratch reused across frames.
	bandOf []int8
	stack  []int32
	areas  []Area
	fired  []Firing
}

// NewEvaluator returns an evaluator of p with no history.
func NewEvaluator(p *Program) *Evaluator {
	return &Evaluator{p: p, states: make([]eventState, len(p.events))}
}

const (
	noBand  = -1
	visited = -2
)

// checkGrid returns a grid's size when it carries values.
func checkGrid(g *datapb.Grid) (int, int, error) {
	w, h, bpl := int(g.GetWidth()), int(g.GetHeight()), int(g.GetBytesPerLine())
	switch {
	case w <= 0 || h <= 0 || w > maxGridSide || h > maxGridSide:
		return 0, 0, fmt.Errorf("grid is %dx%d; a rule reads grids of 1 to %d cells a side", w, h, maxGridSide)
	case bpl < 2*w:
		return 0, 0, fmt.Errorf("grid line of %d bytes is shorter than its %d cells", bpl, w)
	case len(g.GetData()) < bpl*(h-1)+2*w:
		return 0, 0, fmt.Errorf("grid holds %d bytes, fewer than its %dx%d cells", len(g.GetData()), w, h)
	case g.GetScale() == 0 || !finite(g.GetScale()) || !finite(g.GetOffset()):
		return 0, 0, errors.New("grid carries no values (its scale is 0 or not a number)")
	}
	return w, h, nil
}

// cellSpan returns the first and last of n cells along an axis whose centre
// lies within [start, start+size]. last < first when there is none.
func cellSpan(start, size float64, n int) (int, int) {
	first := int(math.Ceil(start*float64(n) - 0.5))
	last := int(math.Floor((start+size)*float64(n) - 0.5))
	return max(first, 0), min(last, n-1)
}

func (p *Program) bandIndex(v float64) int8 {
	for i, b := range p.bands {
		if b.contains(v) {
			return int8(i)
		}
	}
	return noBand
}

// Evaluate runs the rule on one grid frame taken at frameNanos, which drives
// the events' timing. A grid without values returns an error and leaves the
// timing as it was; the next good frame then counts the gap.
func (e *Evaluator) Evaluate(g *datapb.Grid, frameNanos int64) (Frame, error) {
	w, h, err := checkGrid(g)
	if err != nil {
		return Frame{}, err
	}
	c0, c1 := cellSpan(e.p.region.X, e.p.region.Width, w)
	r0, r1 := cellSpan(e.p.region.Y, e.p.region.Height, h)
	data, bpl := g.GetData(), int(g.GetBytesPerLine())
	scale, offset := g.GetScale(), g.GetOffset()
	value := func(col, row int) float64 {
		return float64(binary.LittleEndian.Uint16(data[row*bpl+2*col:]))*scale + offset
	}
	if n := w * h; cap(e.bandOf) < n {
		e.bandOf = make([]int8, n)
	} else {
		e.bandOf = e.bandOf[:n]
	}

	frame := Frame{RegionMax: math.Inf(-1), RegionMin: math.Inf(1)}
	for row := r0; row <= r1; row++ {
		for col := c0; col <= c1; col++ {
			v := value(col, row)
			frame.RegionMax = max(frame.RegionMax, v)
			frame.RegionMin = min(frame.RegionMin, v)
			e.bandOf[row*w+col] = e.p.bandIndex(v)
			frame.Cells++
		}
	}

	// Areas: 8-connected components of one band, flood-filled with an
	// explicit stack. Diagonal neighbours count, so a thin hot object at an
	// angle, such as a pipe or a cable, is one area and not a row of single
	// cells that min_cells would each drop. Two objects that touch only at a
	// corner merge, which is the safer mistake for an alarm.
	fw, fh := float64(w), float64(h)
	e.areas = e.areas[:0]
	for row := r0; row <= r1; row++ {
		for col := c0; col <= c1; col++ {
			b := e.bandOf[row*w+col]
			if b < 0 {
				continue
			}
			e.bandOf[row*w+col] = visited
			e.stack = append(e.stack[:0], int32(row*w+col))
			cells, sum, top := 0, 0.0, math.Inf(-1)
			topAt, minC, maxC, minR, maxR := 0, col, col, row, row
			for len(e.stack) > 0 {
				i := int(e.stack[len(e.stack)-1])
				e.stack = e.stack[:len(e.stack)-1]
				ci, ri := i%w, i/w
				v := value(ci, ri)
				cells++
				sum += v
				if v > top {
					top, topAt = v, i
				}
				minC, maxC, minR, maxR = min(minC, ci), max(maxC, ci), min(minR, ri), max(maxR, ri)
				for dr := -1; dr <= 1; dr++ {
					for dc := -1; dc <= 1; dc++ {
						nc, nr := ci+dc, ri+dr
						if nc < c0 || nc > c1 || nr < r0 || nr > r1 {
							continue
						}
						if j := nr*w + nc; e.bandOf[j] == b {
							e.bandOf[j] = visited
							e.stack = append(e.stack, int32(j))
						}
					}
				}
			}
			if cells < e.p.minCells {
				continue
			}
			bd := e.p.bands[b]
			e.areas = append(e.areas, Area{
				Band: bd.name, Color: bd.color, band: int(b), Cells: cells,
				Max: top, Mean: sum / float64(cells),
				MaxX: (float64(topAt%w) + 0.5) / fw, MaxY: (float64(topAt/w) + 0.5) / fh,
				Left: float64(minC) / fw, Top: float64(minR) / fh,
				Right: float64(maxC+1) / fw, Bottom: float64(maxR+1) / fh,
			})
		}
	}
	slices.SortStableFunc(e.areas, func(a, b Area) int { return cmp.Compare(b.Max, a.Max) })
	frame.Areas = e.areas

	e.fired = e.fired[:0]
	for k := range e.p.events {
		ev := &e.p.events[k]
		match := -1
		for i := range e.areas {
			if ev.cond.matches(&e.areas[i]) {
				match = i // the first is the one with the highest value
				break
			}
		}
		var clears bool
		switch {
		case frame.Cells == 0:
			clears = true
		case ev.clearBelow != nil:
			clears = frame.RegionMax < *ev.clearBelow
		case ev.clearAbove != nil:
			clears = frame.RegionMin > *ev.clearAbove
		default:
			clears = match < 0
		}
		if e.states[k].step(frameNanos, match >= 0, clears, ev.forNanos, ev.cooldown) {
			e.fired = append(e.fired, Firing{Event: ev.name, Area: e.areas[match], FrameNanos: frameNanos})
		}
	}
	frame.Fired = e.fired
	return frame, nil
}

// eventState is one event's timing on one stream, in frame time.
type eventState struct {
	seen, raised, pending, fired bool
	// last is the previous frame's time, since when the condition has held
	// while pending, and lastFired when the event last fired.
	last, since, lastFired int64
}

// step advances the event by one frame at t, given whether the condition
// holds and whether the event's clear condition holds, and reports whether the
// event fires on this frame.
func (s *eventState) step(t int64, holds, clears bool, forNanos, cooldown int64) bool {
	if s.seen && (t < s.last || t-s.last > MaxFrameGap.Nanoseconds()) {
		// The frames do not show what happened in between, so nothing that
		// was timed before the gap counts. Only when the event last fired
		// survives, so the gap cannot cut a cooldown short; after the clock
		// steps back, the cooldown restarts from this frame.
		s.raised, s.pending = false, false
		if s.fired && t < s.lastFired {
			s.lastFired = t
		}
	}
	s.seen, s.last = true, t
	if s.raised {
		if !clears {
			return false
		}
		s.raised = false
	}
	if !holds {
		s.pending = false
		return false
	}
	if !s.pending {
		s.pending, s.since = true, t
	}
	if t-s.since < forNanos {
		return false
	}
	// A condition that keeps holding through the cooldown fires as soon as
	// the cooldown ends.
	if s.fired && t-s.lastFired < max(cooldown, MinEventSpacing.Nanoseconds()) {
		return false
	}
	s.raised, s.pending, s.fired, s.lastFired = true, false, true, t
	return true
}

// Markers draws a frame's areas on the grid's picture: a box per area, in its
// band's colour, labelled with the band and the area's highest value in the
// grid's unit, so a viewer shows for example "hot 52.3 °C". An empty set
// clears the boxes. The markers name the grid's camera and carry its
// correction, as the grid's values do.
func Markers(f Frame, g *datapb.Grid, validFor time.Duration) *datapb.VideoMarkers {
	m := &datapb.VideoMarkers{Camera: g.GetCamera(), ValidForNanos: int64(validFor), Correction: g.GetCorrection()}
	areas := f.Areas[:min(len(f.Areas), MaxMarkers)]
	if len(areas) > 0 {
		m.Markers = make([]*datapb.Marker, 0, len(areas))
	}
	for _, a := range areas {
		x, y, width, height := a.Box()
		m.Markers = append(m.Markers, &datapb.Marker{
			Kind: datapb.Marker_KIND_BOX,
			X:    float32(x), Y: float32(y), Width: float32(width), Height: float32(height),
			Label: a.Band, Value: proto.Float64(a.Max), Unit: g.GetUnit(), Color: a.Color,
		})
	}
	return m
}
