package data

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// ObjectDescriptor describes one object the world view searches camera frames
// for, as a format rather than a trained class: a shape, a metric size, a colour
// palette, or a label an application's model already emits. Each attribute
// scores a candidate region and fusion combines the scores into one confidence.
// The agent's world view job (services/data_worldview.go) runs the search.
type ObjectDescriptor struct {
	Event string `json:"event" yaml:"event"`
	// Rate is frames per second per camera; EveryFrames is the alternative
	// that samples every Nth frame. Exactly one is set.
	Rate        float64                     `json:"rate,omitempty" yaml:"rate,omitempty"`
	EveryFrames int                         `json:"every_frames,omitempty" yaml:"every_frames,omitempty"`
	ClearAfter  string                      `json:"clear_after" yaml:"clear_after"`
	Cooldown    string                      `json:"cooldown" yaml:"cooldown"`
	Fusion      ObjectFusion                `json:"fusion" yaml:"fusion"`
	Attributes  map[string]*ObjectAttribute `json:"attributes" yaml:"attributes"`
	Composition []ObjectPrimitive           `json:"composition,omitempty" yaml:"composition,omitempty"`
	Enabled     *bool                       `json:"enabled,omitempty" yaml:"enabled,omitempty"`
}

// ObjectFusion combines attribute scores. Threshold is the fused confidence a
// candidate must reach; Required names attributes whose own score must also
// clear that attribute's veto floor, whatever the fused score is.
type ObjectFusion struct {
	Threshold float64  `json:"threshold" yaml:"threshold"`
	Required  []string `json:"required,omitempty" yaml:"required,omitempty"`
}

// ObjectAttribute is one scored property of an object. The key it is stored
// under in ObjectDescriptor.Attributes is its kind, which selects how Expect is
// read; see objectAttributeKinds.
type ObjectAttribute struct {
	Expect map[string]any `json:"expect" yaml:"expect"`
	Weight float64        `json:"weight" yaml:"weight"`
	// Min is the veto floor when the attribute is in fusion.required. Nil
	// means unset and takes defaultObjectVetoFloor; an explicit 0 is kept as
	// 0, so a declared floor is never replaced by the default.
	Min *float64 `json:"min,omitempty" yaml:"min,omitempty"`
}

// defaultObjectVetoFloor applies to a required attribute that declares no min.
const defaultObjectVetoFloor = 0.5

// VetoFloor returns the declared min, or the default when none is declared.
func (a *ObjectAttribute) VetoFloor() float64 {
	if a == nil || a.Min == nil {
		return defaultObjectVetoFloor
	}
	return *a.Min
}

// ObjectPrimitive is one part of an object built from several simple solids,
// for example a bottle as a cylinder body, a cone shoulder and a cylinder neck.
type ObjectPrimitive struct {
	Primitive string     `json:"primitive" yaml:"primitive"`
	WidthM    [2]float64 `json:"w_m" yaml:"w_m"`
	HeightM   [2]float64 `json:"h_m" yaml:"h_m"`
}

// CampaignDepth names the depth source that turns pixel extents into metres,
// which the size attribute needs.
type CampaignDepth struct {
	Source     string          `json:"source" yaml:"source"`
	ScaleM     float64         `json:"scale_m" yaml:"scale_m"`
	Intrinsics DepthIntrinsics `json:"intrinsics" yaml:"intrinsics"`
}

// DepthIntrinsics are the pinhole camera intrinsics in pixels: the focal
// lengths fx and fy and the principal point cx, cy.
type DepthIntrinsics struct {
	Fx float64 `json:"fx" yaml:"fx"`
	Fy float64 `json:"fy" yaml:"fy"`
	Cx float64 `json:"cx" yaml:"cx"`
	Cy float64 `json:"cy" yaml:"cy"`
}

// ObjectsStatus is live state, never part of the campaign revision or plan.
type ObjectsStatus struct {
	State             string            `json:"state"`
	Error             string            `json:"error,omitempty"`
	Sources           map[string]string `json:"sources,omitempty"`
	NotificationError string            `json:"notification_error,omitempty"`
}

func (o *ObjectDescriptor) IsEnabled() bool { return o != nil && (o.Enabled == nil || *o.Enabled) }
func (o *ObjectDescriptor) ClearDuration() time.Duration {
	d, _ := time.ParseDuration(o.ClearAfter)
	return d
}
func (o *ObjectDescriptor) CooldownDuration() time.Duration {
	d, _ := time.ParseDuration(o.Cooldown)
	return d
}

var objectNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

const (
	maxObjects           = 32
	maxObjectComposition = 8
	maxObjectPaletteSize = 8
	// maxObjectExtentM bounds every metric range; nothing a camera search is
	// meant to find is larger than this.
	maxObjectExtentM = 10.0
	// defaultColourTolerance is the colour difference a palette entry may
	// differ by when the colour attribute declares no tolerance, as a delta E
	// in the International Commission on Illumination 1976 (CIE76) formula.
	defaultColourTolerance = 25.0
)

var objectPrimitives = []string{"cylinder", "box", "sphere", "cone"}

// objectAttributeKinds validates each attribute kind's expect map. A new kind
// is one entry here; an attribute whose kind is not listed is rejected.
var objectAttributeKinds = map[string]func(expect map[string]any) error{
	"shape":  validateShapeExpect,
	"size":   validateSizeExpect,
	"colour": validateColourExpect,
	"class":  validateClassExpect,
}

func supportedObjectAttributeKinds() string {
	kinds := make([]string, 0, len(objectAttributeKinds))
	for kind := range objectAttributeKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return strings.Join(kinds, ", ")
}

func sortedObjectNames(objects map[string]*ObjectDescriptor) []string {
	names := make([]string, 0, len(objects))
	for name := range objects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (c Campaign) validateObjects() error {
	if c.Objects == nil {
		if c.Depth != nil {
			return errors.New("depth requires objects")
		}
		return nil
	}
	if len(c.Objects) == 0 || len(c.Objects) > maxObjects {
		return fmt.Errorf("objects must define 1..%d objects", maxObjects)
	}
	camera := false
	for _, source := range c.Sources {
		camera = camera || source.Camera != ""
	}
	if !camera {
		return errors.New("objects require a camera source")
	}
	events := map[string]string{}
	for _, name := range sortedObjectNames(c.Objects) {
		object := c.Objects[name]
		if !objectNameRE.MatchString(name) {
			return fmt.Errorf("objects name %q must be a lowercase letter followed by up to 63 lowercase letters, numbers or '_'", name)
		}
		if object == nil {
			return fmt.Errorf("objects.%s must be a mapping", name)
		}
		if err := object.validate("objects." + name); err != nil {
			return err
		}
		if other, taken := events[object.Event]; taken {
			return fmt.Errorf("objects.%s.event %q is already used by objects.%s", name, object.Event, other)
		}
		if c.Inference != nil && object.Event == c.Inference.Event {
			return fmt.Errorf("objects.%s.event %q is already used by inference.event", name, object.Event)
		}
		events[object.Event] = name
	}
	return c.Depth.validate()
}

func (o *ObjectDescriptor) validate(path string) error {
	if !inferenceEventRE.MatchString(o.Event) {
		return fmt.Errorf("%s.event must use 1..128 letters, numbers, '.', '-' or '_'", path)
	}
	if (o.Rate != 0) == (o.EveryFrames != 0) {
		return fmt.Errorf("%s must set exactly one of rate or every_frames", path)
	}
	if o.Rate != 0 && (math.IsNaN(o.Rate) || math.IsInf(o.Rate, 0) || o.Rate <= 0 || o.Rate > 30) {
		return fmt.Errorf("%s.rate must be in (0, 30] frames per second per camera", path)
	}
	if o.EveryFrames != 0 && (o.EveryFrames < 1 || o.EveryFrames > 300) {
		return fmt.Errorf("%s.every_frames must be in [1, 300]", path)
	}
	for _, field := range []struct{ name, raw string }{{"clear_after", o.ClearAfter}, {"cooldown", o.Cooldown}} {
		d, err := time.ParseDuration(field.raw)
		if err != nil || d <= 0 || d > 24*time.Hour {
			return fmt.Errorf("%s.%s must be a positive duration of at most 24h", path, field.name)
		}
	}
	if t := o.Fusion.Threshold; math.IsNaN(t) || t <= 0 || t > 1 {
		return fmt.Errorf("%s.fusion.threshold must be in (0, 1]", path)
	}
	if len(o.Attributes) == 0 {
		return fmt.Errorf("%s.attributes must define at least one attribute", path)
	}
	kinds := make([]string, 0, len(o.Attributes))
	for kind := range o.Attributes {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	for _, kind := range kinds {
		attribute := o.Attributes[kind]
		validateExpect, known := objectAttributeKinds[kind]
		if !known {
			return fmt.Errorf("%s.attributes: unknown attribute kind %q; supported kinds are %s", path, kind, supportedObjectAttributeKinds())
		}
		at := path + ".attributes." + kind
		if attribute == nil {
			return fmt.Errorf("%s must be a mapping with expect and weight", at)
		}
		if w := attribute.Weight; math.IsNaN(w) || math.IsInf(w, 0) || w <= 0 {
			return fmt.Errorf("%s.weight must be a finite number greater than 0", at)
		}
		if m := attribute.Min; m != nil && (math.IsNaN(*m) || *m < 0 || *m > 1) {
			return fmt.Errorf("%s.min must be in [0, 1]", at)
		}
		if err := validateExpect(attribute.Expect); err != nil {
			return fmt.Errorf("%s.expect: %w", at, err)
		}
	}
	seen := map[string]bool{}
	for _, required := range o.Fusion.Required {
		if _, ok := o.Attributes[required]; !ok {
			return fmt.Errorf("%s.fusion.required names %q, which is not one of this object's attributes", path, required)
		}
		if seen[required] {
			return fmt.Errorf("%s.fusion.required lists %q more than once", path, required)
		}
		seen[required] = true
	}
	if o.Composition != nil {
		if len(o.Composition) == 0 || len(o.Composition) > maxObjectComposition {
			return fmt.Errorf("%s.composition must list 1..%d primitives", path, maxObjectComposition)
		}
		for i, part := range o.Composition {
			at := fmt.Sprintf("%s.composition[%d]", path, i)
			if !isObjectPrimitive(part.Primitive) {
				return fmt.Errorf("%s.primitive must be one of %s", at, strings.Join(objectPrimitives, ", "))
			}
			if err := validateMetricRange("w_m", part.WidthM); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
			if err := validateMetricRange("h_m", part.HeightM); err != nil {
				return fmt.Errorf("%s: %w", at, err)
			}
		}
	}
	return nil
}

func (d *CampaignDepth) validate() error {
	if d == nil {
		return nil
	}
	if strings.TrimSpace(d.Source) == "" {
		return errors.New("depth.source is required")
	}
	if !positiveFinite(d.ScaleM) {
		return errors.New("depth.scale_m must be a finite number greater than 0")
	}
	for _, field := range []struct {
		name  string
		value float64
	}{{"fx", d.Intrinsics.Fx}, {"fy", d.Intrinsics.Fy}, {"cx", d.Intrinsics.Cx}, {"cy", d.Intrinsics.Cy}} {
		if !positiveFinite(field.value) {
			return fmt.Errorf("depth.intrinsics.%s must be a finite number greater than 0", field.name)
		}
	}
	return nil
}

func positiveFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0
}

func isObjectPrimitive(primitive string) bool {
	for _, candidate := range objectPrimitives {
		if candidate == primitive {
			return true
		}
	}
	return false
}

// rejectUnknownExpectKeys keeps a misspelt expect key from being silently
// ignored, since expect is an open map rather than a decoded struct.
func rejectUnknownExpectKeys(expect map[string]any, known ...string) error {
	allowed := map[string]bool{}
	for _, key := range known {
		allowed[key] = true
	}
	var unknown []string
	for key := range expect {
		if !allowed[key] {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("unknown keys %s; this kind accepts %s", strings.Join(unknown, ", "), strings.Join(known, ", "))
	}
	return nil
}

// finiteNumber reads a YAML or JSON number out of an expect map value.
func finiteNumber(value any) (float64, bool) {
	number, ok := numericValue(value)
	if !ok {
		if unsigned, isUnsigned := value.(uint64); isUnsigned {
			number, ok = float64(unsigned), true
		}
	}
	return number, ok && !math.IsNaN(number) && !math.IsInf(number, 0)
}

// numberList reads a list of exactly n finite numbers.
func numberList(value any, n int) ([]float64, bool) {
	list, ok := value.([]any)
	if !ok || len(list) != n {
		return nil, false
	}
	numbers := make([]float64, n)
	for i, item := range list {
		if numbers[i], ok = finiteNumber(item); !ok {
			return nil, false
		}
	}
	return numbers, true
}

// expectRange reads a [min, max] pair with 0 < min <= max <= limit.
func expectRange(expect map[string]any, key string, limit float64) error {
	numbers, ok := numberList(expect[key], 2)
	if !ok {
		return fmt.Errorf("%s must be a [min, max] pair of numbers", key)
	}
	return validateMetricRangeLimit(key, [2]float64{numbers[0], numbers[1]}, limit)
}

func validateMetricRange(key string, bounds [2]float64) error {
	return validateMetricRangeLimit(key, bounds, maxObjectExtentM)
}

func validateMetricRangeLimit(key string, bounds [2]float64, limit float64) error {
	low, high := bounds[0], bounds[1]
	if math.IsNaN(low) || math.IsNaN(high) || low <= 0 || low > high || high > limit {
		return fmt.Errorf("%s must be a [min, max] pair with 0 < min <= max <= %g", key, limit)
	}
	return nil
}

func validateShapeExpect(expect map[string]any) error {
	if err := rejectUnknownExpectKeys(expect, "primitive", "aspect"); err != nil {
		return err
	}
	primitive, _ := expect["primitive"].(string)
	if !isObjectPrimitive(primitive) {
		return fmt.Errorf("primitive must be one of %s", strings.Join(objectPrimitives, ", "))
	}
	if _, ok := expect["aspect"]; ok {
		if err := expectRange(expect, "aspect", math.MaxFloat64); err != nil {
			return errors.New("aspect must be a [min, max] pair with 0 < min <= max")
		}
	}
	return nil
}

func validateSizeExpect(expect map[string]any) error {
	if err := rejectUnknownExpectKeys(expect, "w_m", "h_m", "d_m"); err != nil {
		return err
	}
	for _, key := range []string{"w_m", "h_m"} {
		if err := expectRange(expect, key, maxObjectExtentM); err != nil {
			return err
		}
	}
	if _, ok := expect["d_m"]; ok {
		return expectRange(expect, "d_m", maxObjectExtentM)
	}
	return nil
}

func validateColourExpect(expect map[string]any) error {
	if err := rejectUnknownExpectKeys(expect, "palette", "tolerance"); err != nil {
		return err
	}
	palette, ok := expect["palette"].([]any)
	if !ok || len(palette) == 0 || len(palette) > maxObjectPaletteSize {
		return fmt.Errorf("palette must list 1..%d entries of {lab: [L, a, b], share: 0..1}", maxObjectPaletteSize)
	}
	total := 0.0
	for i, raw := range palette {
		entry, ok := raw.(map[string]any)
		if !ok {
			return fmt.Errorf("palette[%d] must be a mapping {lab: [L, a, b], share: 0..1}", i)
		}
		if err := rejectUnknownExpectKeys(entry, "lab", "share"); err != nil {
			return fmt.Errorf("palette[%d]: %w", i, err)
		}
		lab, ok := numberList(entry["lab"], 3)
		if !ok || lab[0] < 0 || lab[0] > 100 || lab[1] < -128 || lab[1] > 127 || lab[2] < -128 || lab[2] > 127 {
			return fmt.Errorf("palette[%d].lab must be [L, a, b] with L in 0..100 and a and b in -128..127", i)
		}
		share, ok := finiteNumber(entry["share"])
		if !ok || share < 0 || share > 1 {
			return fmt.Errorf("palette[%d].share must be a number from 0 through 1", i)
		}
		total += share
	}
	if total > 1+1e-9 {
		return fmt.Errorf("palette shares must sum to at most 1, not %g", total)
	}
	if raw, ok := expect["tolerance"]; ok {
		tolerance, ok := finiteNumber(raw)
		if !ok || tolerance <= 0 || tolerance > 100 {
			return errors.New("tolerance must be a CIE76 delta E greater than 0 and at most 100")
		}
	}
	return nil
}

// ColourTolerance returns the colour attribute's declared CIE76 delta E, or the
// default when it declares none.
func (a *ObjectAttribute) ColourTolerance() float64 {
	if a != nil {
		if tolerance, ok := finiteNumber(a.Expect["tolerance"]); ok {
			return tolerance
		}
	}
	return defaultColourTolerance
}

func validateClassExpect(expect map[string]any) error {
	if err := rejectUnknownExpectKeys(expect, "source", "label", "model"); err != nil {
		return err
	}
	if source, _ := expect["source"].(string); source != "app" {
		return errors.New(`source must be "app"`)
	}
	label, _ := expect["label"].(string)
	if label == "" || len(label) > 128 {
		return errors.New("label must be a nonempty string of at most 128 bytes")
	}
	if raw, ok := expect["model"]; ok {
		if model, isString := raw.(string); !isString || model == "" {
			return errors.New("model must be a nonempty string")
		}
	}
	return nil
}

// objectsDigestInput is the author-declared objects plan the revision digest
// covers. It is called only when objects are declared.
func (c Campaign) objectsDigestInput() map[string]any {
	objects := make(map[string]any, len(c.Objects))
	for name, object := range c.Objects {
		attributes := make(map[string]any, len(object.Attributes))
		for kind, attribute := range object.Attributes {
			// Min is hashed as the pointer, as enabled is, so an absent min and
			// an explicit one are different plans.
			attributes[kind] = map[string]any{"expect": attribute.Expect, "weight": attribute.Weight, "min": attribute.Min}
		}
		// Absent and empty required lists mean the same thing and hash alike.
		required := append([]string{}, object.Fusion.Required...)
		composition := make([]map[string]any, 0, len(object.Composition))
		for _, part := range object.Composition {
			composition = append(composition, map[string]any{"primitive": part.Primitive, "w_m": part.WidthM, "h_m": part.HeightM})
		}
		objects[name] = map[string]any{
			"event": object.Event, "rate": object.Rate, "every_frames": object.EveryFrames,
			"clear_after": object.ClearAfter, "cooldown": object.Cooldown,
			"fusion":     map[string]any{"threshold": object.Fusion.Threshold, "required": required},
			"attributes": attributes, "composition": composition, "enabled": object.Enabled,
		}
	}
	return objects
}

func (d *CampaignDepth) digestInput() map[string]any {
	return map[string]any{
		"source": d.Source, "scale_m": d.ScaleM,
		"intrinsics": map[string]any{"fx": d.Intrinsics.Fx, "fy": d.Intrinsics.Fy, "cx": d.Intrinsics.Cx, "cy": d.Intrinsics.Cy},
	}
}
