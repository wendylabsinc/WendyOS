package gridrules

import (
	"bytes"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// exampleRule is the rule of the design: yellow from 40, red from 50, alert
// on red.
const exampleRule = `
name: hotspot-alarm
input: agent/camera/grid/v4l2:/dev/video2
steps:
  - region: {x: 0, y: 0, width: 1, height: 1}
  - bands:
      - {name: warm, min: 40, max: 50, color: yellow}
      - {name: hot,  min: 50,          color: red}
  - areas: {min_cells: 4}
outputs:
  markers: areas
  events:
    - {name: thermal_warning, when: area.band == warm, for: 3s}
    - {name: thermal_alarm,   when: area.band == hot,  for: 2s, clear_below: 48, cooldown: 2m}
`

// decode reads a rule as campaign YAML does: unknown fields are refused.
func decode(t *testing.T, raw string) (Rule, error) {
	t.Helper()
	var rule Rule
	decoder := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	decoder.KnownFields(true)
	err := decoder.Decode(&rule)
	return rule, err
}

func mustRule(t *testing.T, raw string) Rule {
	t.Helper()
	rule, err := decode(t, raw)
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func mustCompile(t *testing.T, raw string) *Program {
	t.Helper()
	p, err := mustRule(t, raw).Compile()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestExampleRuleCompiles(t *testing.T) {
	p := mustCompile(t, exampleRule)
	if p.Name() != "hotspot-alarm" || p.Input() != "agent/camera/grid/v4l2:/dev/video2" || !p.Markers() {
		t.Fatalf("program %+v", p)
	}
	if p.region != (Region{X: 0, Y: 0, Width: 1, Height: 1}) || p.minCells != 4 || len(p.bands) != 2 || len(p.events) != 2 {
		t.Fatalf("program %+v", p)
	}
	if hot := p.bands[1]; hot.name != "hot" || hot.min != 50 || !strings.HasPrefix(hot.color, "red") || hot.contains(1e300) != true {
		t.Fatalf("hot band %+v", hot)
	}
	alarm := p.events[1]
	if alarm.name != "thermal_alarm" || alarm.cond.field != "band" || alarm.cond.band != 1 || alarm.forNanos != 2e9 || alarm.cooldown != 120e9 || *alarm.clearBelow != 48 {
		t.Fatalf("alarm %+v", alarm)
	}
}

// Without a region the rule reads the whole grid, and without an areas step
// every area counts, down to one cell.
func TestDefaults(t *testing.T) {
	p := mustCompile(t, `
name: r
input: agent/camera/grid/x
steps:
  - bands: [{name: dark, max: 10}]
outputs: {markers: areas}
`)
	if p.region != (Region{Width: 1, Height: 1}) || p.minCells != 1 || len(p.events) != 0 {
		t.Fatalf("program %+v", p)
	}
}

// Bad rules are refused with a message that names the field and says what
// would be accepted.
func TestValidationRefusesBadRules(t *testing.T) {
	base := func(steps, outputs string) string {
		return "name: r\ninput: agent/camera/grid/x\nsteps:\n" + steps + "outputs:\n" + outputs
	}
	bands := "  - bands: [{name: warm, min: 40, max: 50}, {name: hot, min: 50}]\n"
	events := func(e string) string { return "  events:\n    - " + e + "\n" }
	for _, tc := range []struct{ name, yaml, want string }{
		{"no name", strings.Replace(base(bands, "  markers: areas\n"), "name: r", "name: ''", 1), "name must be"},
		{"slash in name", strings.Replace(base(bands, "  markers: areas\n"), "name: r", "name: a/b", 1), "name must be"},
		{"no input", strings.Replace(base(bands, "  markers: areas\n"), "input: agent/camera/grid/x", "input: ''", 1), "input must name a grid stream"},
		{"no steps", "name: r\ninput: x\noutputs: {markers: areas}\n", "steps must list a bands step"},
		{"two kinds in one step", base("  - bands: [{name: a, min: 1}]\n    areas: {min_cells: 2}\n", "  markers: areas\n"), "steps[0] must be exactly one of region, bands or areas"},
		{"empty step", base("  - {}\n"+bands, "  markers: areas\n"), "steps[0] must be exactly one"},
		{"out of order", base(bands+"  - region: {x: 0, y: 0, width: 1, height: 1}\n", "  markers: areas\n"), "steps[1] (region): the steps run in the order region, bands, areas"},
		{"bands twice", base(bands+bands, "  markers: areas\n"), "steps[1] (bands): the steps run in the order"},
		{"no bands", base("  - areas: {min_cells: 2}\n", "  markers: areas\n"), "must include a bands step"},
		{"empty bands", base("  - bands: []\n", "  markers: areas\n"), "steps[0].bands must list 1 to 8 bands"},
		{"region outside", base("  - region: {x: 0.5, y: 0, width: 0.6, height: 1}\n"+bands, "  markers: areas\n"), "steps[0].region: the region must lie inside the grid"},
		{"region without size", base("  - region: {x: 0, y: 0}\n"+bands, "  markers: areas\n"), "width and height must be above 0"},
		{"region negative", base("  - region: {x: -0.1, y: 0, width: 0.5, height: 1}\n"+bands, "  markers: areas\n"), "x and y must be from 0"},
		{"band without bounds", base("  - bands: [{name: warm}]\n", "  markers: areas\n"), "steps[0].bands[0] (warm) needs min, max or both"},
		{"band inverted", base("  - bands: [{name: warm, min: 50, max: 40}]\n", "  markers: areas\n"), "min 50 must be below max 40"},
		{"bands overlap", base("  - bands: [{name: warm, min: 40, max: 51}, {name: hot, min: 50}]\n", "  markers: areas\n"), "steps[0].bands[1] (hot) overlaps band warm"},
		{"band twice", base("  - bands: [{name: warm, min: 40, max: 50}, {name: warm, min: 60}]\n", "  markers: areas\n"), "band warm is listed twice"},
		{"band colour", base("  - bands: [{name: warm, min: 40, color: 'red; x'}]\n", "  markers: areas\n"), "color must be a CSS colour name or #RRGGBB"},
		{"min cells", base(bands+"  - areas: {min_cells: -1}\n", "  markers: areas\n"), "steps[1].areas.min_cells must be from 1"},
		{"markers kind", base(bands, "  markers: points\n"), `outputs.markers "points" is not supported`},
		{"no outputs", base(bands, "  events: []\n"), "a rule without outputs does nothing"},
		{"event name", base(bands, events("{name: 'bad name', when: area.band == hot}")), "outputs.events[0]: name must be"},
		{"event twice", base(bands, "  events:\n    - {name: a, when: area.band == hot}\n    - {name: a, when: area.band == warm}\n"), "outputs.events[1]: event a is listed twice"},
		{"unknown band", base(bands, events("{name: a, when: area.band == hott}")), "no band is named hott; the bands are warm, hot"},
		{"single equals", base(bands, events("{name: a, when: area.band = hot}")), "compare with ==, not ="},
		{"not equals", base(bands, events("{name: a, when: area.band != hot}")), "!= is not supported"},
		{"band order", base(bands, events("{name: a, when: area.band > hot}")), "area.band can only be compared with =="},
		{"unknown field", base(bands, events("{name: a, when: area.size > 3}")), "area.size is not a field"},
		{"not an area", base(bands, events("{name: a, when: max > 3}")), "must compare one area field"},
		{"max word", base(bands, events("{name: a, when: area.max > hot}")), "area.max must be compared with a number"},
		{"cells fraction", base(bands, events("{name: a, when: area.cells >= 2.5}")), "whole number of cells"},
		{"for", base(bands, events("{name: a, when: area.band == hot, for: 3}")), "for must be a duration from 0s to 24h, such as 3s or 2m"},
		{"for negative", base(bands, events("{name: a, when: area.band == hot, for: -1s}")), "for must be a duration"},
		{"cooldown long", base(bands, events("{name: a, when: area.band == hot, cooldown: 25h}")), "cooldown must be a duration"},
		{"clear both", base(bands, events("{name: a, when: area.band == hot, clear_below: 48, clear_above: 60}")), "set clear_below or clear_above, not both"},
		{"clear above band", base(bands, events("{name: a, when: area.band == hot, clear_below: 52}")), "clear_below 52 is above band hot's min, 50, so the event would clear while its condition still holds; set clear_below at or below 50"},
		{"clear above threshold", base(bands, events("{name: a, when: area.max >= 60, clear_below: 61}")), "clear_below 61 is above the threshold in when, 60"},
		{"clear above for a low band", base(bands, events("{name: a, when: area.band == warm, clear_above: 45}")), "clear_above 45 is below band warm's max, 50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rule, err := decode(t, tc.yaml)
			if err == nil {
				err = rule.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v; want it to contain %q", err, tc.want)
			}
		})
	}
}

// A key a rule does not know is refused, inside a step too, so a typo such as
// min_cell is not silently ignored.
func TestUnknownKeysAreRefused(t *testing.T) {
	for _, raw := range []string{
		strings.Replace(exampleRule, "min_cells: 4", "min_cell: 4", 1),
		strings.Replace(exampleRule, "color: red", "colour: red", 1),
		strings.Replace(exampleRule, "cooldown: 2m", "cool_down: 2m", 1),
		strings.Replace(exampleRule, "  - areas:", "  - blobs:", 1),
	} {
		if _, err := decode(t, raw); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("unknown key accepted: %v", err)
		}
	}
}

// Clear thresholds that hysteresis allows: at or below the band's min, and an
// open band's clear_above.
func TestValidHysteresis(t *testing.T) {
	for _, e := range []string{
		"{name: a, when: area.band == hot, clear_below: 50}",
		"{name: a, when: area.band == hot, clear_below: 48}",
		"{name: a, when: area.max > 60, clear_below: 55}",
		"{name: a, when: area.band == warm, clear_above: 50}",
		"{name: a, when: area.band == cold, clear_above: 12}",
		"{name: a, when: area.mean < 3, clear_above: 100}",
		`{name: a, when: "area.band == 'hot'"}`,
		"{name: a, when: area.cells >= 10, for: 0s, cooldown: 0s}",
	} {
		raw := "name: r\ninput: agent/camera/grid/x\nsteps:\n  - bands: [{name: cold, max: 10}, {name: warm, min: 40, max: 50}, {name: hot, min: 50}]\noutputs:\n  events:\n    - " + e + "\n"
		if _, err := mustRule(t, raw).Compile(); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
	}
}

// The digest covers what the author wrote, so a change to any of it is a new
// revision, and it does not change with Go's struct layout.
func TestDigestInputCoversTheRule(t *testing.T) {
	rule := mustRule(t, exampleRule)
	d := rule.DigestInput()
	steps := d["steps"].([]map[string]any)
	if d["name"] != "hotspot-alarm" || d["input"] != rule.Input || len(steps) != 3 || steps[0]["region"] == nil || steps[2]["areas"] == nil {
		t.Fatalf("digest %v", d)
	}
	bands := steps[1]["bands"].([]map[string]any)
	if bands[1]["name"] != "hot" || bands[1]["color"] != "red" || bands[1]["max"].(*float64) != nil {
		t.Fatalf("bands %v", bands)
	}
	events := d["outputs"].(map[string]any)["events"].([]map[string]any)
	if events[1]["cooldown"] != "2m" || *events[1]["clear_below"].(*float64) != 48 {
		t.Fatalf("events %v", events)
	}
}
