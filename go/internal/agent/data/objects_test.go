package data

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// worldViewCampaignYAML declares two world view objects, a can described by
// shape, size and colour and a bottle built from three primitives, plus the
// depth source the size attribute needs.
const worldViewCampaignYAML = `version: 1
name: shelf-watch
sources:
  - camera: front
objects:
  coke_can:
    event: coke_can_seen
    rate: 2
    clear_after: 5s
    cooldown: 30s
    fusion:
      threshold: 0.75
      required: [shape, colour]
    attributes:
      shape:
        expect: {primitive: cylinder, aspect: [1.6, 2.1]}
        weight: 1
      size:
        expect: {w_m: [0.06, 0.07], h_m: [0.11, 0.13]}
        weight: 0.5
      colour:
        expect:
          palette:
            - {lab: [45, 65, 45], share: 0.6}
            - {lab: [95, 0, 0], share: 0.2}
          tolerance: 20
        weight: 1.5
        min: 0.6
  bottle:
    event: bottle_seen
    every_frames: 15
    clear_after: 10s
    cooldown: 1m
    fusion: {threshold: 0.6}
    attributes:
      class:
        expect: {source: app, label: bottle, model: detector}
        weight: 2
      size:
        expect: {w_m: [0.05, 0.1], h_m: [0.2, 0.35], d_m: [0.05, 0.1]}
        weight: 1
    composition:
      - {primitive: cylinder, w_m: [0.06, 0.09], h_m: [0.12, 0.2]}
      - {primitive: cone, w_m: [0.02, 0.09], h_m: [0.03, 0.06]}
      - {primitive: cylinder, w_m: [0.02, 0.03], h_m: [0.02, 0.05]}
    enabled: false
depth:
  source: front-depth
  scale_m: 0.001
  intrinsics: {fx: 615.2, fy: 615.9, cx: 320.5, cy: 240.25}
capture:
  buffer: 1s
  after_trigger: 5s
  triggers:
    - object.coke_can.confidence: "> 0.8"
upload: {when: wifi}
export: {annotation: cvat}
`

func parseWorldView(t *testing.T, contents string) Campaign {
	t.Helper()
	campaign, err := ParseCampaign([]byte(contents))
	if err != nil {
		t.Fatal(err)
	}
	return campaign
}

func TestParseCampaignObjectsRoundTrip(t *testing.T) {
	campaign := parseWorldView(t, worldViewCampaignYAML)
	falseValue := false
	want := map[string]*ObjectDescriptor{
		"coke_can": {
			Event: "coke_can_seen", Rate: 2, ClearAfter: "5s", Cooldown: "30s",
			Fusion: ObjectFusion{Threshold: 0.75, Required: []string{"shape", "colour"}},
			Attributes: map[string]*ObjectAttribute{
				"shape": {Expect: map[string]any{"primitive": "cylinder", "aspect": []any{1.6, 2.1}}, Weight: 1},
				"size":  {Expect: map[string]any{"w_m": []any{0.06, 0.07}, "h_m": []any{0.11, 0.13}}, Weight: 0.5},
				"colour": {Expect: map[string]any{
					"palette": []any{
						map[string]any{"lab": []any{45, 65, 45}, "share": 0.6},
						map[string]any{"lab": []any{95, 0, 0}, "share": 0.2},
					},
					"tolerance": 20,
				}, Weight: 1.5, Min: 0.6},
			},
		},
		"bottle": {
			Event: "bottle_seen", EveryFrames: 15, ClearAfter: "10s", Cooldown: "1m",
			Fusion: ObjectFusion{Threshold: 0.6},
			Attributes: map[string]*ObjectAttribute{
				"class": {Expect: map[string]any{"source": "app", "label": "bottle", "model": "detector"}, Weight: 2},
				"size":  {Expect: map[string]any{"w_m": []any{0.05, 0.1}, "h_m": []any{0.2, 0.35}, "d_m": []any{0.05, 0.1}}, Weight: 1},
			},
			Composition: []ObjectPrimitive{
				{Primitive: "cylinder", WidthM: [2]float64{0.06, 0.09}, HeightM: [2]float64{0.12, 0.2}},
				{Primitive: "cone", WidthM: [2]float64{0.02, 0.09}, HeightM: [2]float64{0.03, 0.06}},
				{Primitive: "cylinder", WidthM: [2]float64{0.02, 0.03}, HeightM: [2]float64{0.02, 0.05}},
			},
			Enabled: &falseValue,
		},
	}
	if !reflect.DeepEqual(campaign.Objects, want) {
		got, _ := json.MarshalIndent(campaign.Objects, "", "  ")
		t.Fatalf("objects did not round-trip:\n%s", got)
	}
	wantDepth := &CampaignDepth{Source: "front-depth", ScaleM: 0.001, Intrinsics: DepthIntrinsics{Fx: 615.2, Fy: 615.9, Cx: 320.5, Cy: 240.25}}
	if !reflect.DeepEqual(campaign.Depth, wantDepth) {
		t.Fatalf("depth = %+v, want %+v", campaign.Depth, wantDepth)
	}
	if got := campaign.Capture.Triggers[0].ObjectConfidence; !reflect.DeepEqual(got, map[string]string{"coke_can": "> 0.8"}) {
		t.Fatalf("object trigger = %v", got)
	}
	if !campaign.Objects["coke_can"].IsEnabled() || campaign.Objects["bottle"].IsEnabled() {
		t.Fatal("enabled defaults to true and honors an explicit false")
	}
	if campaign.Objects["coke_can"].Attributes["colour"].VetoFloor() != 0.6 || campaign.Objects["coke_can"].Attributes["shape"].VetoFloor() != 0.5 {
		t.Fatal("veto floor is the declared min, or 0.5 when none is declared")
	}
	if campaign.Objects["coke_can"].Attributes["colour"].ColourTolerance() != 20 {
		t.Fatal("declared colour tolerance was not read")
	}

	// The stored plan is JSON. Everything the author declared must survive the
	// trip through it, and live status must never enter the digest.
	stored, err := json.Marshal(campaign)
	if err != nil {
		t.Fatal(err)
	}
	var restored Campaign
	if err := json.Unmarshal(stored, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.Depth, campaign.Depth) || !reflect.DeepEqual(restored.Capture.Triggers, campaign.Capture.Triggers) || len(restored.Objects) != 2 {
		t.Fatalf("stored plan lost world view fields: %s", stored)
	}
	if restored.Objects["bottle"].Composition[1] != campaign.Objects["bottle"].Composition[1] || restored.Objects["bottle"].IsEnabled() {
		t.Fatalf("stored bottle lost fields: %+v", restored.Objects["bottle"])
	}
	restoredDigest, _ := json.Marshal(restored.planDigestInput())
	originalDigest, _ := json.Marshal(campaign.planDigestInput())
	if string(restoredDigest) != string(originalDigest) {
		t.Fatalf("the stored plan hashes differently from the parsed one:\n%s\n%s", restoredDigest, originalDigest)
	}
	campaign.ObjectsStatus = &ObjectsStatus{State: "error", Error: "transient runtime error"}
	plan, err := json.Marshal(campaign.planDigestInput())
	if err != nil || strings.Contains(string(plan), "objects_status") || strings.Contains(string(plan), "transient") {
		t.Fatalf("live status leaked into plan: %s: %v", plan, err)
	}
}

func TestParseCampaignObjectsRejections(t *testing.T) {
	for _, tc := range []struct {
		name, old, new, want string
	}{
		{"object name with capitals", "  coke_can:\n    event", "  CokeCan:\n    event", "objects name \"CokeCan\""},
		{"object name too long", "  coke_can:\n    event", "  " + strings.Repeat("a", 65) + ":\n    event", "must be a lowercase letter"},
		{"missing event", "    event: coke_can_seen\n", "", "objects.coke_can.event must use"},
		{"malformed event", "event: coke_can_seen", "event: coke can seen", "objects.coke_can.event must use"},
		{"duplicate event", "event: bottle_seen", "event: coke_can_seen", "is already used by objects.bottle"},
		{"rate and every_frames", "    rate: 2\n", "    rate: 2\n    every_frames: 3\n", "exactly one of rate or every_frames"},
		{"neither rate nor every_frames", "    rate: 2\n", "", "exactly one of rate or every_frames"},
		{"rate above 30", "rate: 2", "rate: 31", "objects.coke_can.rate must be in (0, 30]"},
		{"negative rate", "rate: 2", "rate: -1", "objects.coke_can.rate must be in (0, 30]"},
		{"every_frames above 300", "every_frames: 15", "every_frames: 301", "objects.bottle.every_frames must be in [1, 300]"},
		{"negative every_frames", "every_frames: 15", "every_frames: -1", "objects.bottle.every_frames must be in [1, 300]"},
		{"clear_after not positive", "clear_after: 5s", "clear_after: 0s", "objects.coke_can.clear_after must be a positive duration of at most 24h"},
		{"cooldown above 24h", "cooldown: 30s", "cooldown: 25h", "objects.coke_can.cooldown must be a positive duration of at most 24h"},
		{"fusion threshold zero", "threshold: 0.75", "threshold: 0", "objects.coke_can.fusion.threshold must be in (0, 1]"},
		{"fusion threshold above 1", "threshold: 0.75", "threshold: 1.5", "objects.coke_can.fusion.threshold must be in (0, 1]"},
		{"required names no attribute", "required: [shape, colour]", "required: [shape, class]", "fusion.required names \"class\""},
		{"required duplicate", "required: [shape, colour]", "required: [shape, shape]", "lists \"shape\" more than once"},
		{"no attributes", "    fusion: {threshold: 0.6}\n    attributes:\n      class:\n        expect: {source: app, label: bottle, model: detector}\n        weight: 2\n      size:\n        expect: {w_m: [0.05, 0.1], h_m: [0.2, 0.35], d_m: [0.05, 0.1]}\n        weight: 1\n", "    fusion: {threshold: 0.6}\n    attributes: {}\n", "objects.bottle.attributes must define at least one attribute"},
		{"zero weight", "weight: 0.5", "weight: 0", "objects.coke_can.attributes.size.weight must be a finite number greater than 0"},
		{"infinite weight", "weight: 0.5", "weight: .inf", "objects.coke_can.attributes.size.weight must be a finite number greater than 0"},
		{"min above 1", "min: 0.6", "min: 1.2", "objects.coke_can.attributes.colour.min must be in [0, 1]"},
		{"unknown attribute kind", "      shape:\n        expect: {primitive: cylinder", "      texture:\n        expect: {primitive: cylinder", "unknown attribute kind \"texture\"; supported kinds are class, colour, shape, size"},
		{"unknown shape key", "{primitive: cylinder, aspect: [1.6, 2.1]}", "{primitive: cylinder, aspect: [1.6, 2.1], sides: 3}", "unknown keys sides"},
		{"unknown shape primitive", "{primitive: cylinder, aspect", "{primitive: torus, aspect", "primitive must be one of cylinder, box, sphere, cone"},
		{"aspect reversed", "aspect: [1.6, 2.1]", "aspect: [2.1, 1.6]", "aspect must be a [min, max] pair with 0 < min <= max"},
		{"aspect not a pair", "aspect: [1.6, 2.1]", "aspect: 2", "aspect must be a [min, max] pair"},
		{"size missing h_m", "{w_m: [0.06, 0.07], h_m: [0.11, 0.13]}", "{w_m: [0.06, 0.07]}", "h_m must be a [min, max] pair of numbers"},
		{"size above 10 metres", "{w_m: [0.06, 0.07], h_m", "{w_m: [0.06, 11], h_m", "w_m must be a [min, max] pair with 0 < min <= max <= 10"},
		{"size zero minimum", "{w_m: [0.06, 0.07], h_m", "{w_m: [0, 0.07], h_m", "w_m must be a [min, max] pair with 0 < min <= max <= 10"},
		{"size bad d_m", "d_m: [0.05, 0.1]", "d_m: [0.1, 0.05]", "d_m must be a [min, max] pair"},
		{"unknown size key", "{w_m: [0.06, 0.07], h_m: [0.11, 0.13]}", "{w_m: [0.06, 0.07], h_m: [0.11, 0.13], volume_l: 0.33}", "unknown keys volume_l"},
		{"empty palette", "          palette:\n            - {lab: [45, 65, 45], share: 0.6}\n            - {lab: [95, 0, 0], share: 0.2}\n", "          palette: []\n", "palette must list 1..8 entries"},
		{"palette too long", "            - {lab: [95, 0, 0], share: 0.2}\n", strings.Repeat("            - {lab: [95, 0, 0], share: 0.01}\n", 8), "palette must list 1..8 entries"},
		{"lightness above 100", "{lab: [45, 65, 45]", "{lab: [101, 65, 45]", "palette[0].lab must be [L, a, b]"},
		{"a below -128", "{lab: [45, 65, 45]", "{lab: [45, -129, 45]", "palette[0].lab must be [L, a, b]"},
		{"lab with two components", "{lab: [45, 65, 45]", "{lab: [45, 65]", "palette[0].lab must be [L, a, b]"},
		{"share above 1", "share: 0.6}", "share: 1.2}", "palette[0].share must be a number from 0 through 1"},
		{"shares sum above 1", "share: 0.6}", "share: 0.9}", "palette shares must sum to at most 1"},
		{"unknown palette entry key", "share: 0.6}", "share: 0.6, name: red}", "palette[0]: unknown keys name"},
		{"tolerance zero", "tolerance: 20", "tolerance: 0", "tolerance must be a CIE76 delta E"},
		{"tolerance above 100", "tolerance: 20", "tolerance: 101", "tolerance must be a CIE76 delta E"},
		{"unknown colour key", "tolerance: 20", "tolerance: 20\n          space: srgb", "unknown keys space"},
		{"class source not app", "{source: app, label: bottle", "{source: model, label: bottle", "source must be \"app\""},
		{"class label empty", "label: bottle,", "label: \"\",", "label must be a nonempty string of at most 128 bytes"},
		{"class label too long", "label: bottle,", "label: " + strings.Repeat("b", 129) + ",", "label must be a nonempty string of at most 128 bytes"},
		{"class model not a string", "model: detector}", "model: [detector]}", "model must be a nonempty string"},
		{"unknown class key", "model: detector}", "model: detector, threshold: 0.5}", "unknown keys threshold"},
		{"empty composition", "    composition:\n      - {primitive: cylinder, w_m: [0.06, 0.09], h_m: [0.12, 0.2]}\n      - {primitive: cone, w_m: [0.02, 0.09], h_m: [0.03, 0.06]}\n      - {primitive: cylinder, w_m: [0.02, 0.03], h_m: [0.02, 0.05]}\n", "    composition: []\n", "objects.bottle.composition must list 1..8 primitives"},
		{"composition too long", "      - {primitive: cone, w_m: [0.02, 0.09], h_m: [0.03, 0.06]}\n", strings.Repeat("      - {primitive: cone, w_m: [0.02, 0.09], h_m: [0.03, 0.06]}\n", 7), "objects.bottle.composition must list 1..8 primitives"},
		{"composition unknown primitive", "{primitive: cone,", "{primitive: pyramid,", "objects.bottle.composition[1].primitive must be one of"},
		{"composition bad range", "{primitive: cone, w_m: [0.02, 0.09]", "{primitive: cone, w_m: [0.09, 0.02]", "objects.bottle.composition[1]: w_m must be a [min, max] pair"},
		{"composition range above 10 metres", "h_m: [0.03, 0.06]}", "h_m: [0.03, 12]}", "objects.bottle.composition[1]: h_m must be a [min, max] pair with 0 < min <= max <= 10"},
		{"no camera source", "  - camera: front\n", "  - telemetry: true\n", "objects require a camera source"},
		{"depth without source", "  source: front-depth\n", "", "depth.source is required"},
		{"depth scale not positive", "scale_m: 0.001", "scale_m: 0", "depth.scale_m must be a finite number greater than 0"},
		{"depth intrinsic not positive", "cx: 320.5", "cx: 0", "depth.intrinsics.cx must be a finite number greater than 0"},
		{"depth intrinsic missing", "fx: 615.2, ", "", "depth.intrinsics.fx must be a finite number greater than 0"},
		{"unknown depth key", "  scale_m: 0.001\n", "  scale_m: 0.001\n  baseline_m: 0.05\n", "field baseline_m not found"},
		{"unknown object key", "    cooldown: 30s\n", "    cooldown: 30s\n    priority: 1\n", "field priority not found"},
		{"trigger names undefined object", "object.coke_can.confidence", "object.pepsi_can.confidence", "object.pepsi_can.confidence names an object that objects does not define"},
		{"trigger confidence above 1", `"> 0.8"`, `"> 1.5"`, "object.coke_can.confidence must compare with a number from 0 through 1"},
		{"trigger confidence without operator", `"> 0.8"`, `"0.8"`, "object.coke_can.confidence must begin with"},
		{"unknown trigger key", "    - object.coke_can.confidence: \"> 0.8\"\n", "    - object.coke_can.score: \"> 0.8\"\n", `unknown capture trigger key "object.coke_can.score"`},
		{"trigger with two forms", "    - object.coke_can.confidence: \"> 0.8\"\n", "    - object.coke_can.confidence: \"> 0.8\"\n      event: go\n", "must define exactly one of event, model.uncertainty or object.<name>.confidence"},
		{"trigger with two objects", "    - object.coke_can.confidence: \"> 0.8\"\n", "    - object.coke_can.confidence: \"> 0.8\"\n      object.bottle.confidence: \"> 0.8\"\n", "must define exactly one of event, model.uncertainty or object.<name>.confidence"},
		{"duplicate trigger key", "    - object.coke_can.confidence: \"> 0.8\"\n", "    - object.coke_can.confidence: \"> 0.8\"\n      object.coke_can.confidence: \"> 0.9\"\n", "already defined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(worldViewCampaignYAML, tc.old) {
				t.Fatalf("test fixture does not contain %q", tc.old)
			}
			_, err := ParseCampaign([]byte(strings.Replace(worldViewCampaignYAML, tc.old, tc.new, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseCampaignObjectsCrossChecks(t *testing.T) {
	// depth without objects.
	withDepth := strings.Replace(drainDigestPinYAML, "upload:", "depth: {source: d, scale_m: 1, intrinsics: {fx: 1, fy: 1, cx: 1, cy: 1}}\nupload:", 1)
	if _, err := ParseCampaign([]byte(withDepth)); err == nil || !strings.Contains(err.Error(), "depth requires objects") {
		t.Fatalf("depth without objects: %v", err)
	}
	// An object trigger in a campaign that declares no objects at all.
	noObjects := strings.Replace(drainDigestPinYAML, "- event: pinned", "- object.coke_can.confidence: \"> 0.8\"", 1)
	if _, err := ParseCampaign([]byte(noObjects)); err == nil || !strings.Contains(err.Error(), "names an object that objects does not define") {
		t.Fatalf("object trigger without objects: %v", err)
	}
	// An empty objects block.
	emptyObjects := strings.Replace(drainDigestPinYAML, "upload:", "objects: {}\nupload:", 1)
	if _, err := ParseCampaign([]byte(emptyObjects)); err == nil || !strings.Contains(err.Error(), "objects must define 1..32 objects") {
		t.Fatalf("empty objects: %v", err)
	}
	// More than 32 objects.
	var many strings.Builder
	many.WriteString("objects:\n")
	for i := 0; i < 33; i++ {
		many.WriteString("  o" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + ":\n    event: e" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "\n    rate: 1\n    clear_after: 1s\n    cooldown: 1s\n    fusion: {threshold: 0.5}\n    attributes: {shape: {expect: {primitive: box}, weight: 1}}\n")
	}
	tooMany := strings.Replace(strings.Replace(drainDigestPinYAML, "telemetry: true", "camera: front", 1), "upload:", many.String()+"upload:", 1)
	if _, err := ParseCampaign([]byte(tooMany)); err == nil || !strings.Contains(err.Error(), "objects must define 1..32 objects") {
		t.Fatalf("33 objects: %v", err)
	}
	// An object event that collides with inference.event.
	people := string(peopleCampaign(t))
	collision := strings.Replace(people, "\ncapture:\n  buffer", "\nobjects:\n  person:\n    event: person_detected\n    rate: 1\n    clear_after: 1s\n    cooldown: 1s\n    fusion: {threshold: 0.5}\n    attributes: {shape: {expect: {primitive: box}, weight: 1}}\ncapture:\n  buffer", 1)
	if _, err := ParseCampaign([]byte(collision)); err == nil || !strings.Contains(err.Error(), "is already used by inference.event") {
		t.Fatalf("object event colliding with inference.event: %v", err)
	}
	distinct := strings.Replace(collision, "    event: person_detected\n    rate: 1", "    event: person_shape_seen\n    rate: 1", 1)
	if _, err := ParseCampaign([]byte(distinct)); err != nil {
		t.Fatalf("object beside inference with a distinct event was rejected: %v", err)
	}
	// A colour attribute with no tolerance takes the default.
	defaulted := parseWorldView(t, strings.Replace(worldViewCampaignYAML, "          tolerance: 20\n", "", 1))
	if got := defaulted.Objects["coke_can"].Attributes["colour"].ColourTolerance(); got != 25 {
		t.Fatalf("default colour tolerance = %g, want 25", got)
	}
}

// TestObjectsLeaveExistingDigestsUnchanged is the reason objects and depth
// enter the digest only when declared. drainDigestPinYAML and its pinned
// revision predate the world view (pinned at 8378e2319, the feature/world-view
// base), and the people example exercises the inference and notify branches.
func TestObjectsLeaveExistingDigestsUnchanged(t *testing.T) {
	pinned := parseWorldView(t, drainDigestPinYAML)
	if pinned.Revision != drainDigestPinRevision {
		t.Fatalf("a campaign without objects now hashes to %s, want the pinned %s", pinned.Revision, drainDigestPinRevision)
	}
	// Computed on the untouched base, 8378e2319, before this change existed.
	const peopleRevision = "5bdd06fff93d5871fd29b77c85a8a9f17d28486b06c2a4af4d21fa610f1ca625"
	if people := parseWorldView(t, string(peopleCampaign(t))); people.Revision != peopleRevision {
		t.Fatalf("the people example now hashes to %s, want %s", people.Revision, peopleRevision)
	}
	plan, err := json.Marshal(pinned.planDigestInput())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"objects"`, `"depth"`, `"object_confidence"`} {
		if strings.Contains(string(plan), key) {
			t.Fatalf("a campaign without objects carries %s in its digest input: %s", key, plan)
		}
	}
}

func TestObjectsSettingsChangeCampaignRevision(t *testing.T) {
	base := parseWorldView(t, worldViewCampaignYAML)
	for _, change := range [][2]string{
		{"weight: 0.5", "weight: 0.25"},
		{"event: coke_can_seen", "event: can_seen"},
		{"rate: 2", "rate: 3"},
		{"every_frames: 15", "every_frames: 16"},
		{"clear_after: 5s", "clear_after: 6s"},
		{"cooldown: 30s", "cooldown: 31s"},
		{"threshold: 0.75", "threshold: 0.7"},
		{"required: [shape, colour]", "required: [shape]"},
		{"min: 0.6", "min: 0.7"},
		{"{primitive: cylinder, aspect: [1.6, 2.1]}", "{primitive: cylinder, aspect: [1.5, 2.1]}"},
		{"tolerance: 20", "tolerance: 21"},
		{"{lab: [45, 65, 45], share: 0.6}", "{lab: [46, 65, 45], share: 0.6}"},
		{"{primitive: cone, w_m: [0.02, 0.09]", "{primitive: cone, w_m: [0.03, 0.09]"},
		{"    enabled: false\n", "    enabled: true\n"},
		{"    enabled: false\n", ""},
		{"source: front-depth", "source: rear-depth"},
		{"scale_m: 0.001", "scale_m: 0.002"},
		{"fx: 615.2", "fx: 600"},
		{`"> 0.8"`, `"> 0.85"`},
	} {
		t.Run(change[0]+" -> "+change[1], func(t *testing.T) {
			if !strings.Contains(worldViewCampaignYAML, change[0]) {
				t.Fatalf("test fixture does not contain %q", change[0])
			}
			changed := parseWorldView(t, strings.Replace(worldViewCampaignYAML, change[0], change[1], 1))
			if changed.Revision == base.Revision {
				t.Fatal("world view plan change did not change revision")
			}
		})
	}
	// Spelling that means the same plan hashes the same.
	for _, same := range [][2]string{
		{"tolerance: 20", "tolerance: 20.0"},
		{"    fusion: {threshold: 0.6}\n", "    fusion: {threshold: 0.6, required: []}\n"},
	} {
		if parseWorldView(t, strings.Replace(worldViewCampaignYAML, same[0], same[1], 1)).Revision != base.Revision {
			t.Errorf("%q and %q hash differently", same[0], same[1])
		}
	}
}

func TestObjectConfidenceTriggerMatch(t *testing.T) {
	campaign := parseWorldView(t, worldViewCampaignYAML)
	sighting := func(model, object string, confidence any) ApplicationRecord {
		return ApplicationRecord{Type: "prediction", Model: model, Attributes: map[string]any{"object": object, "confidence": confidence}}
	}
	reason, expression, matched := campaign.Match(sighting("worldview", "coke_can", 0.9))
	if !matched || reason != "object_confidence:coke_can:0.9" || expression != "object.coke_can.confidence > 0.8" {
		t.Fatalf("confident sighting: %q %q %v", reason, expression, matched)
	}
	for name, record := range map[string]ApplicationRecord{
		"low confidence":          sighting("worldview", "coke_can", 0.7),
		"another object":          sighting("worldview", "bottle", 0.95),
		"another model":           sighting("detector", "coke_can", 0.95),
		"non-numeric confidence":  sighting("worldview", "coke_can", "high"),
		"event record":            {Type: "event", Name: "coke_can", Model: "worldview", Attributes: map[string]any{"object": "coke_can", "confidence": 0.95}},
		"event named like object": {Type: "event", Name: "object.coke_can.confidence"},
	} {
		if reason, _, matched := campaign.Match(record); matched {
			t.Errorf("%s matched as %q", name, reason)
		}
	}
	// A confidence decoded from application JSON arrives as json.Number.
	if _, _, matched := campaign.Match(sighting("worldview", "coke_can", json.Number("0.95"))); !matched {
		t.Error("a json.Number confidence did not match")
	}
}

// TestExistingTriggersMatchAsBefore keeps the hand-written trigger decoder
// from changing what event and model.uncertainty triggers do.
func TestExistingTriggersMatchAsBefore(t *testing.T) {
	campaign := parseWorldView(t, exampleCampaignYAML)
	want := []CampaignTrigger{{Event: "emergency_stop"}, {ModelUncertainty: "> 0.65"}}
	if !reflect.DeepEqual(campaign.Capture.Triggers, want) {
		t.Fatalf("triggers = %+v, want %+v", campaign.Capture.Triggers, want)
	}
	reason, expression, matched := campaign.Match(ApplicationRecord{Type: "prediction", Model: "detector", Attributes: map[string]any{"uncertainty": 0.7}})
	if !matched || reason != "model_uncertainty:0.7" || expression != "model.uncertainty > 0.65" {
		t.Fatalf("uncertainty: %q %q %v", reason, expression, matched)
	}
	reason, expression, matched = campaign.Match(ApplicationRecord{Type: "event", Name: "emergency_stop"})
	if !matched || reason != "event:emergency_stop" || expression != "event=emergency_stop" {
		t.Fatalf("event: %q %q %v", reason, expression, matched)
	}
	// The out-of-range message model.uncertainty always gave is unchanged.
	bad := strings.Replace(exampleCampaignYAML, `"> 0.65"`, `"> 1.5"`, 1)
	if _, err := ParseCampaign([]byte(bad)); err == nil || !strings.HasSuffix(err.Error(), "capture.triggers[1]: model.uncertainty must compare with a number from 0 through 1") {
		t.Fatalf("model.uncertainty range error changed: %v", err)
	}
	// A trigger that is not a mapping is still refused.
	scalar := strings.Replace(exampleCampaignYAML, "    - event: emergency_stop\n", "    - emergency_stop\n", 1)
	if _, err := ParseCampaign([]byte(scalar)); err == nil || !strings.Contains(err.Error(), "must be a mapping") {
		t.Fatalf("scalar trigger: %v", err)
	}
}

func TestDeployWarnsObjectsDoNotRunYet(t *testing.T) {
	manager, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	campaign, err := manager.DeployCampaign([]byte(worldViewCampaignYAML))
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, warning := range campaign.Warnings {
		warned = warned || strings.Contains(warning, "does not run the world view search yet")
	}
	if !warned {
		t.Fatalf("deploy did not warn that objects do not run: %v", campaign.Warnings)
	}
	stored, err := manager.Campaign(campaign.Name)
	if err != nil || stored.Revision != campaign.Revision || len(stored.Objects) != 2 || stored.Depth == nil {
		t.Fatalf("stored campaign lost world view fields: %+v, %v", stored, err)
	}
}
