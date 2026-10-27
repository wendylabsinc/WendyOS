// Package gridrules evaluates simple rules on wendy.data.v1.Grid frames,
// without an application: "40 to 50 is a yellow area, above 50 a red one,
// alert when a red area lasts two seconds".
//
// A rule is a short pipeline of generic steps, then its outputs:
//
//	region  the part of the grid the rule looks at, normalised 0..1
//	bands   named value ranges, each with a colour
//	areas   connected cells of one band, at least min_cells of them
//	outputs markers (a box per area) and named events with timing
//
// Nothing here knows what the values mean. A cell's value is the grid's own
// count * scale + offset, in the grid's unit, so the same rule reads a thermal
// camera's degrees Celsius, a camera's brightness in percent or a depth
// camera's metres. Only the producer of a grid knows what it measured.
//
// A rule is evaluated on every grid frame its producer makes, by its
// producer's goroutine: Evaluate is synchronous and allocates nothing per
// frame once its buffers have grown to the grid's size. Its timing (for,
// clear_below, cooldown) runs on the frames' own timestamps, never on when
// the evaluation happened to run.
package gridrules

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Rule is one rule as a plan declares it, for example in a campaign's rules
// block. Validate or Compile checks it.
type Rule struct {
	// Name identifies the rule within its plan; it appears in stream ids.
	Name string `json:"name" yaml:"name"`
	// Input is the id of the grid stream the rule reads.
	Input   string  `json:"input" yaml:"input"`
	Steps   []Step  `json:"steps" yaml:"steps"`
	Outputs Outputs `json:"outputs" yaml:"outputs"`
}

// Step is one step of a rule. Exactly one field is set. The steps run in the
// order region, bands, areas; bands is required, and each appears at most
// once.
type Step struct {
	Region *Region `json:"region,omitempty" yaml:"region,omitempty"`
	Bands  []Band  `json:"bands,omitempty" yaml:"bands,omitempty"`
	Areas  *Areas  `json:"areas,omitempty" yaml:"areas,omitempty"`
}

// Region is the part of the grid a rule reads: a rectangle normalised to 0..1
// with its top-left corner at (x, y), in the convention of wendy.data.v1
// positions (the origin is the top-left corner of the top-left cell). A cell
// belongs to the region when its centre lies inside it. Without a region step
// the rule reads the whole grid.
type Region struct {
	X      float64 `json:"x" yaml:"x"`
	Y      float64 `json:"y" yaml:"y"`
	Width  float64 `json:"width" yaml:"width"`
	Height float64 `json:"height" yaml:"height"`
}

// Band is a named range of values, min <= value < max, in the grid's unit.
// Either bound may be left out, not both. Bands may not overlap, so a cell
// belongs to at most one band.
type Band struct {
	Name string   `json:"name" yaml:"name"`
	Min  *float64 `json:"min,omitempty" yaml:"min,omitempty"`
	Max  *float64 `json:"max,omitempty" yaml:"max,omitempty"`
	// Color is the band's default colour for viewers: a CSS colour name or
	// #RRGGBB. Empty leaves it to the viewer.
	Color string `json:"color,omitempty" yaml:"color,omitempty"`
}

// Areas groups cells of one band that touch, including diagonally, into
// areas, and drops areas of fewer than MinCells cells. Without an areas step,
// or with min_cells left out, every area counts, down to one cell.
type Areas struct {
	MinCells int `json:"min_cells,omitempty" yaml:"min_cells,omitempty"`
}

// Outputs are what a rule produces.
type Outputs struct {
	// Markers is "areas" for a box per area on the rule's markers stream, or
	// empty for none.
	Markers string  `json:"markers,omitempty" yaml:"markers,omitempty"`
	Events  []Event `json:"events,omitempty" yaml:"events,omitempty"`
}

// Event is a named event a rule raises when its condition has held for For,
// at most once per Cooldown. While raised it raises nothing more; it clears
// when the condition no longer holds, or, with ClearBelow or ClearAbove, once
// every value in the region is below or above that value (hysteresis).
type Event struct {
	Name string `json:"name" yaml:"name"`
	// When is the condition, on the areas of one frame:
	//
	//	area.band == <band>                  an area of that band exists
	//	area.max|area.mean <op> <number>     an area whose highest or mean value compares
	//	area.cells <op> <integer>            an area of that many cells
	//
	// <op> is one of <, <=, ==, >=, >.
	When       string   `json:"when" yaml:"when"`
	For        string   `json:"for,omitempty" yaml:"for,omitempty"`
	ClearBelow *float64 `json:"clear_below,omitempty" yaml:"clear_below,omitempty"`
	ClearAbove *float64 `json:"clear_above,omitempty" yaml:"clear_above,omitempty"`
	Cooldown   string   `json:"cooldown,omitempty" yaml:"cooldown,omitempty"`
}

const (
	// MaxBands, MaxEvents bound one rule.
	MaxBands  = 8
	MaxEvents = 8
	// MaxTiming bounds for and cooldown.
	MaxTiming = 24 * time.Hour
	// MaxFrameGap is the longest gap between two frames that still counts as
	// continuous. A longer gap, or a frame time that goes backwards, starts
	// every event's timing over: the frames do not show what happened in
	// between. A raised event is no longer raised after it, so a condition
	// that still holds raises the event again, cooldown permitting.
	MaxFrameGap = 5 * time.Second
	// MinEventSpacing is the least time between two events of one rule event,
	// whatever its cooldown, so a condition that flickers at frame rate
	// cannot flood the event journal and notifications.
	MinEventSpacing = time.Second
)

var (
	ruleNameRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
	bandNameRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,31}$`)
	eventNameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	colorRE     = regexp.MustCompile(`^(#[0-9A-Fa-f]{6}|[A-Za-z]{1,32})$`)
	whenRE      = regexp.MustCompile(`^area\.([a-z]+)\s*(==|>=|<=|>|<|!=|=)\s*(.+)$`)
)

// Program is a compiled, valid rule. It never changes, so evaluators share it.
type Program struct {
	name     string
	input    string
	region   Region
	bands    []band
	minCells int
	markers  bool
	events   []eventProgram
}

// Name is the rule's name.
func (p *Program) Name() string { return p.name }

// Input is the id of the grid stream the rule reads.
func (p *Program) Input() string { return p.input }

// Markers reports whether the rule publishes its areas as markers.
func (p *Program) Markers() bool { return p.markers }

type band struct {
	name, color string
	// min and max are -Inf and +Inf where the band has no bound.
	min, max float64
}

func (b band) contains(v float64) bool { return v >= b.min && v < b.max }

type eventProgram struct {
	name       string
	cond       condition
	forNanos   int64
	cooldown   int64
	clearBelow *float64
	clearAbove *float64
}

// condition is an event's when, on one area.
type condition struct {
	field string // band, max, mean or cells
	op    string
	band  int
	value float64
}

func (c condition) matches(a *Area) bool {
	switch c.field {
	case "band":
		return a.band == c.band
	case "max":
		return compare(a.Max, c.op, c.value)
	case "mean":
		return compare(a.Mean, c.op, c.value)
	default:
		return compare(float64(a.Cells), c.op, c.value)
	}
}

func compare(v float64, op string, threshold float64) bool {
	switch op {
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	case "==":
		return v == threshold
	case ">=":
		return v >= threshold
	default:
		return v > threshold
	}
}

// Validate reports the first problem with the rule, naming the field, or nil.
func (r Rule) Validate() error {
	_, err := r.Compile()
	return err
}

// Compile validates the rule and returns its program.
func (r Rule) Compile() (*Program, error) {
	if !ruleNameRE.MatchString(r.Name) {
		return nil, errors.New("name must be 1..64 letters, numbers, '.', '-' or '_', starting with a letter or number")
	}
	if r.Input == "" || len(r.Input) > 480 || strings.ContainsAny(r.Input, " \t\r\n\x00") {
		return nil, errors.New("input must name a grid stream, such as agent/camera/grid/v4l2:/dev/video2")
	}
	p := &Program{name: r.Name, input: r.Input, region: Region{Width: 1, Height: 1}, minCells: 1}
	if len(r.Steps) == 0 {
		return nil, errors.New("steps must list a bands step, optionally after a region step and before an areas step")
	}
	const (
		stepRegion = iota
		stepBands
		stepAreas
	)
	names := []string{"region", "bands", "areas"}
	last := -1
	for i, step := range r.Steps {
		kind, set := -1, 0
		if step.Region != nil {
			kind, set = stepRegion, set+1
		}
		if step.Bands != nil {
			kind, set = stepBands, set+1
		}
		if step.Areas != nil {
			kind, set = stepAreas, set+1
		}
		if set != 1 {
			return nil, fmt.Errorf("steps[%d] must be exactly one of region, bands or areas, each a list item of its own", i)
		}
		if kind <= last {
			return nil, fmt.Errorf("steps[%d] (%s): the steps run in the order region, bands, areas, each at most once", i, names[kind])
		}
		last = kind
		switch kind {
		case stepRegion:
			if err := step.Region.validate(); err != nil {
				return nil, fmt.Errorf("steps[%d].region: %w", i, err)
			}
			p.region = *step.Region
		case stepBands:
			bands, err := compileBands(step.Bands)
			if err != nil {
				return nil, fmt.Errorf("steps[%d].bands%w", i, err)
			}
			p.bands = bands
		case stepAreas:
			if step.Areas.MinCells < 0 || step.Areas.MinCells > 1<<20 {
				return nil, fmt.Errorf("steps[%d].areas.min_cells must be from 1 to %d", i, 1<<20)
			}
			p.minCells = max(step.Areas.MinCells, 1)
		}
	}
	if p.bands == nil {
		return nil, errors.New("steps must include a bands step: a rule finds areas of named value ranges")
	}
	switch r.Outputs.Markers {
	case "":
	case "areas":
		p.markers = true
	default:
		return nil, fmt.Errorf("outputs.markers %q is not supported; use areas, or leave it out", r.Outputs.Markers)
	}
	if !p.markers && len(r.Outputs.Events) == 0 {
		return nil, errors.New("outputs must set markers: areas, events, or both; a rule without outputs does nothing")
	}
	if len(r.Outputs.Events) > MaxEvents {
		return nil, fmt.Errorf("outputs.events may list at most %d events", MaxEvents)
	}
	seen := map[string]bool{}
	for i, event := range r.Outputs.Events {
		program, err := p.compileEvent(event)
		if err != nil {
			return nil, fmt.Errorf("outputs.events[%d]: %w", i, err)
		}
		if seen[program.name] {
			return nil, fmt.Errorf("outputs.events[%d]: event %s is listed twice", i, program.name)
		}
		seen[program.name] = true
		p.events = append(p.events, program)
	}
	return p, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func (r *Region) validate() error {
	const slack = 1e-9
	switch {
	case !finite(r.X) || !finite(r.Y) || !finite(r.Width) || !finite(r.Height):
		return errors.New("x, y, width and height must be numbers")
	case r.X < 0 || r.X >= 1 || r.Y < 0 || r.Y >= 1:
		return errors.New("x and y must be from 0 to below 1: the region's top-left corner, as a fraction of the grid")
	case r.Width <= 0 || r.Height <= 0:
		return errors.New("width and height must be above 0")
	case r.X+r.Width > 1+slack || r.Y+r.Height > 1+slack:
		return errors.New("the region must lie inside the grid: x + width and y + height at most 1")
	}
	return nil
}

func bound(v *float64, missing float64) float64 {
	if v == nil {
		return missing
	}
	return *v
}

// compileBands checks the bands; an error starts with the index, for the
// caller's "bands" prefix.
func compileBands(in []Band) ([]band, error) {
	if len(in) == 0 || len(in) > MaxBands {
		return nil, fmt.Errorf(" must list 1 to %d bands", MaxBands)
	}
	out := make([]band, 0, len(in))
	for i, b := range in {
		if !bandNameRE.MatchString(b.Name) {
			return nil, fmt.Errorf("[%d].name must be 1..32 letters, numbers, '.', '-' or '_'", i)
		}
		if b.Min == nil && b.Max == nil {
			return nil, fmt.Errorf("[%d] (%s) needs min, max or both", i, b.Name)
		}
		if b.Min != nil && !finite(*b.Min) || b.Max != nil && !finite(*b.Max) {
			return nil, fmt.Errorf("[%d] (%s): min and max must be numbers", i, b.Name)
		}
		lo, hi := bound(b.Min, math.Inf(-1)), bound(b.Max, math.Inf(1))
		if lo >= hi {
			return nil, fmt.Errorf("[%d] (%s): min %g must be below max %g", i, b.Name, lo, hi)
		}
		if b.Color != "" && !colorRE.MatchString(b.Color) {
			return nil, fmt.Errorf("[%d] (%s): color must be a CSS colour name or #RRGGBB", i, b.Name)
		}
		for _, other := range out {
			if other.name == b.Name {
				return nil, fmt.Errorf("[%d]: band %s is listed twice", i, b.Name)
			}
			if lo < other.max && other.min < hi {
				return nil, fmt.Errorf("[%d] (%s) overlaps band %s; a value may fall in at most one band, and a band includes its min but not its max", i, b.Name, other.name)
			}
		}
		out = append(out, band{name: b.Name, color: b.Color, min: lo, max: hi})
	}
	return out, nil
}

func parseTiming(field, raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || d > MaxTiming {
		return 0, fmt.Errorf("%s must be a duration from 0s to 24h, such as 3s or 2m", field)
	}
	return d.Nanoseconds(), nil
}

func (p *Program) compileEvent(e Event) (eventProgram, error) {
	if !eventNameRE.MatchString(e.Name) {
		return eventProgram{}, errors.New("name must be 1..128 letters, numbers, '.', '-' or '_'")
	}
	out := eventProgram{name: e.Name, clearBelow: e.ClearBelow, clearAbove: e.ClearAbove}
	cond, err := p.parseWhen(e.When)
	if err != nil {
		return eventProgram{}, fmt.Errorf("%s: when %q: %w", e.Name, e.When, err)
	}
	out.cond = cond
	if out.forNanos, err = parseTiming("for", e.For); err != nil {
		return eventProgram{}, fmt.Errorf("%s: %w", e.Name, err)
	}
	if out.cooldown, err = parseTiming("cooldown", e.Cooldown); err != nil {
		return eventProgram{}, fmt.Errorf("%s: %w", e.Name, err)
	}
	if e.ClearBelow != nil && e.ClearAbove != nil {
		return eventProgram{}, fmt.Errorf("%s: set clear_below or clear_above, not both", e.Name)
	}
	for _, v := range []*float64{e.ClearBelow, e.ClearAbove} {
		if v != nil && !finite(*v) {
			return eventProgram{}, fmt.Errorf("%s: clear_below and clear_above must be numbers", e.Name)
		}
	}
	// Hysteresis that clears while the condition still holds is not
	// hysteresis: the event would clear and raise again on every frame the
	// cooldown allows. Refuse the cases that can be told from the rule alone.
	if e.ClearBelow != nil {
		limit, what := math.Inf(1), ""
		switch {
		case cond.field == "band" && !math.IsInf(p.bands[cond.band].min, -1):
			limit, what = p.bands[cond.band].min, "band "+p.bands[cond.band].name+"'s min"
		case cond.field == "max" && (cond.op == ">" || cond.op == ">="):
			limit, what = cond.value, "the threshold in when"
		}
		if *e.ClearBelow > limit {
			return eventProgram{}, fmt.Errorf("%s: clear_below %g is above %s, %g, so the event would clear while its condition still holds; set clear_below at or below %g", e.Name, *e.ClearBelow, what, limit, limit)
		}
	}
	if e.ClearAbove != nil {
		limit, what := math.Inf(-1), ""
		switch {
		case cond.field == "band" && !math.IsInf(p.bands[cond.band].max, 1):
			limit, what = p.bands[cond.band].max, "band "+p.bands[cond.band].name+"'s max"
		case cond.field == "max" && (cond.op == "<" || cond.op == "<="):
			limit, what = cond.value, "the threshold in when"
		}
		if *e.ClearAbove < limit {
			return eventProgram{}, fmt.Errorf("%s: clear_above %g is below %s, %g, so the event would clear while its condition still holds; set clear_above at or above %g", e.Name, *e.ClearAbove, what, limit, limit)
		}
	}
	return out, nil
}

func (p *Program) bandNames() string {
	names := make([]string, len(p.bands))
	for i, b := range p.bands {
		names[i] = b.name
	}
	return strings.Join(names, ", ")
}

func (p *Program) parseWhen(raw string) (condition, error) {
	m := whenRE.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return condition{}, errors.New("must compare one area field, such as area.band == hot, area.max > 60 or area.cells >= 10")
	}
	field, op, value := m[1], m[2], strings.TrimSpace(m[3])
	switch op {
	case "=":
		return condition{}, errors.New("compare with ==, not =")
	case "!=":
		return condition{}, errors.New("!= is not supported; compare with <, <=, ==, >= or >")
	}
	switch field {
	case "band":
		if op != "==" {
			return condition{}, errors.New("area.band can only be compared with ==")
		}
		name := strings.Trim(value, `"'`)
		for i, b := range p.bands {
			if b.name == name {
				return condition{field: field, op: op, band: i}, nil
			}
		}
		return condition{}, fmt.Errorf("no band is named %s; the bands are %s", name, p.bandNames())
	case "max", "mean":
		v, err := strconv.ParseFloat(value, 64)
		if err != nil || !finite(v) {
			return condition{}, fmt.Errorf("area.%s must be compared with a number", field)
		}
		return condition{field: field, op: op, value: v}, nil
	case "cells":
		v, err := strconv.Atoi(value)
		if err != nil || v < 0 {
			return condition{}, errors.New("area.cells must be compared with a whole number of cells")
		}
		return condition{field: field, op: op, value: float64(v)}, nil
	default:
		return condition{}, fmt.Errorf("area.%s is not a field; use area.band, area.max, area.mean or area.cells", field)
	}
}

// DigestInput is the rule as a plan's revision digest covers it: every field
// the author wrote, enumerated, so a field added to Rule later does not change
// the revision of a plan that does not use it.
func (r Rule) DigestInput() map[string]any {
	steps := make([]map[string]any, 0, len(r.Steps))
	for _, step := range r.Steps {
		entry := map[string]any{}
		if s := step.Region; s != nil {
			entry["region"] = map[string]any{"x": s.X, "y": s.Y, "width": s.Width, "height": s.Height}
		}
		if step.Bands != nil {
			bands := make([]map[string]any, 0, len(step.Bands))
			for _, b := range step.Bands {
				bands = append(bands, map[string]any{"name": b.Name, "min": b.Min, "max": b.Max, "color": b.Color})
			}
			entry["bands"] = bands
		}
		if s := step.Areas; s != nil {
			entry["areas"] = map[string]any{"min_cells": s.MinCells}
		}
		steps = append(steps, entry)
	}
	events := make([]map[string]any, 0, len(r.Outputs.Events))
	for _, e := range r.Outputs.Events {
		events = append(events, map[string]any{"name": e.Name, "when": e.When, "for": e.For,
			"clear_below": e.ClearBelow, "clear_above": e.ClearAbove, "cooldown": e.Cooldown})
	}
	return map[string]any{
		"name":    r.Name,
		"input":   r.Input,
		"steps":   steps,
		"outputs": map[string]any{"markers": r.Outputs.Markers, "events": events},
	}
}
