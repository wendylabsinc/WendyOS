// Package robotinspect holds the schema a read-only robot inspection produces, and the
// one place that builds, compares and renders it. Both the CLI and the agent depend on
// it: a probe runs on the device and the operator reads the result on a laptop, so a
// divergence between the two spellings of a measurement would surface as a silently
// wrong number rather than an error.
//
// The types here are deliberately awkward to construct. A robot's declared numbers and
// its real ones disagree often enough that the disagreement is the product, and the
// failure this package exists to prevent is a quantity recorded without the context
// needed to compare it: an angle with no axis, or a rate with no sampling window. Those
// cannot be represented. See Quantity and Observation.
package robotinspect

import "fmt"

// Unit is a physical unit plus the qualifiers a value in it cannot go without. Adding a
// unit is the only place that decides what context its values must carry.
type Unit struct {
	symbol        string
	requiresAxis  bool
	requiresFrame bool
}

// Symbol returns the unit's printed symbol.
func (u Unit) Symbol() string { return u.symbol }

// UnitOption declares what a unit's values cannot go without.
type UnitOption func(*Unit)

// RequiresAxis marks a unit whose values are meaningless without the axis they were
// taken along.
func RequiresAxis() UnitOption { return func(u *Unit) { u.requiresAxis = true } }

// RequiresFrame marks a unit whose values are meaningless without a reference frame.
func RequiresFrame() UnitOption { return func(u *Unit) { u.requiresFrame = true } }

// NewUnit declares a unit. A vendor backend introduces its own this way rather than
// editing this package — the same reasoning as Requirement being a plain string. A
// servo bus reporting encoder ticks, or an AGV reporting a payload in kilograms, must
// not need a change here to say so.
func NewUnit(symbol string, opts ...UnitOption) Unit {
	u := Unit{symbol: symbol}
	for _, opt := range opts {
		opt(&u)
	}
	return u
}

// The units the core probes report in. Angular and linear quantities require an axis
// because a robot's declared field of view and its measured one are routinely quoted on
// different axes; a translation additionally requires the frame it is measured in.
//
// Count is for a dimensionless tally and nothing else. Bytes and Pixels exist so that a
// memory size and an image width are not both printed as a bare number that a consumer
// cannot tell apart.
var (
	Degrees         = NewUnit("deg", RequiresAxis())
	Radians         = NewUnit("rad", RequiresAxis())
	Millimetres     = NewUnit("mm", RequiresAxis(), RequiresFrame())
	Metres          = NewUnit("m", RequiresAxis(), RequiresFrame())
	MetresPerSecond = NewUnit("m/s")
	Hertz           = NewUnit("Hz")
	Milliseconds    = NewUnit("ms")
	Celsius         = NewUnit("C")
	Watts           = NewUnit("W")
	Volts           = NewUnit("V")
	Amps            = NewUnit("A")
	NewtonMetres    = NewUnit("Nm")
	Ticks           = NewUnit("ticks")
	// VendorUnits is for a number a robot really publishes whose scale nobody has
	// established. It exists so a reading can be reported without inventing a unit for
	// it: a finger torque of "5861 Nm" is a fabrication, while "5861 vendor units" is
	// the truth plus an admission. A property carrying this is a candidate for someone
	// with the vendor's documentation to convert, not a number to compute with.
	VendorUnits = NewUnit("vendor units")
	Bytes       = NewUnit("B")
	Pixels      = NewUnit("px")
	Percent     = NewUnit("%")
	Count       = NewUnit("count")
)

// Well-known axes. A backend may report any axis string; these are the ones the core
// compares and renders specially.
const (
	AxisHorizontal = "horizontal"
	AxisVertical   = "vertical"
	AxisDiagonal   = "diagonal"
)

// Quantity is a number that carries enough context to be compared with another number
// for the same property. Its fields are unexported so that NewQuantity is the only way
// to make one, and so an under-qualified value fails at the probe rather than in a
// report an operator then trusts.
type Quantity struct {
	value float64
	unit  Unit
	axis  string
	frame string
	// set distinguishes a real zero from an unset quantity. Without it a count of
	// zero equalled Quantity{} and was rejected as having no value, so the rows that
	// matter most — no disk free, no interfaces, no joints found — silently vanished
	// from the report instead of reading zero.
	set bool
}

// QuantityOption supplies a qualifier to NewQuantity.
type QuantityOption func(*Quantity)

// WithAxis records which axis the value is measured along. Required for angular and
// linear units.
func WithAxis(axis string) QuantityOption { return func(q *Quantity) { q.axis = axis } }

// WithFrame records the reference frame the value is expressed in. Required for linear
// units, where a number without a frame names no point on the robot.
func WithFrame(frame string) QuantityOption { return func(q *Quantity) { q.frame = frame } }

// NewQuantity returns a quantity, or an error naming the qualifier the unit requires.
func NewQuantity(value float64, unit Unit, opts ...QuantityOption) (Quantity, error) {
	q := Quantity{value: value, unit: unit, set: true}
	for _, opt := range opts {
		opt(&q)
	}
	if unit.requiresAxis && q.axis == "" {
		return Quantity{}, fmt.Errorf("robotinspect: a value in %s needs an axis", unit.orName())
	}
	if unit.requiresFrame && q.frame == "" {
		return Quantity{}, fmt.Errorf("robotinspect: a value in %s needs a frame", unit.orName())
	}
	return q, nil
}

// MustQuantity is NewQuantity for a value fixed at compile time, such as a figure read
// off a datasheet in a backend. It panics on an under-qualified value, so never call it
// on anything derived from a device.
func MustQuantity(value float64, unit Unit, opts ...QuantityOption) Quantity {
	q, err := NewQuantity(value, unit, opts...)
	if err != nil {
		panic(err)
	}
	return q
}

// Value, Unit, Axis and Frame read a quantity back.
func (q Quantity) Value() float64 { return q.value }
func (q Quantity) Unit() Unit     { return q.unit }
func (q Quantity) Axis() string   { return q.axis }
func (q Quantity) Frame() string  { return q.frame }

// Zero reports whether q was never set, which is only possible for a struct literal
// built outside the constructors. A quantity whose value is zero is not unset.
func (q Quantity) Zero() bool { return !q.set }

// comparableWith reports whether two quantities describe the same thing well enough to
// be checked against each other. Differing axes are the case that matters: a declared
// horizontal field of view and a measured vertical one are both correct and must never
// be reported as a disagreement.
func (q Quantity) comparableWith(other Quantity) bool {
	return q.unit.symbol == other.unit.symbol && q.axis == other.axis && q.frame == other.frame
}

// String renders the value with its unit and axis, which is the only form the core ever
// prints. A bare number never leaves this package.
func (q Quantity) String() string {
	out := trimFloat(q.value)
	if q.unit.symbol != "" {
		out += " " + q.unit.symbol
	}
	if q.axis != "" {
		out += " " + q.axis
	}
	if q.frame != "" {
		out += " in " + q.frame
	}
	return out
}

func (u Unit) orName() string {
	if u.symbol == "" {
		return "this unit"
	}
	return u.symbol
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.3f", f)
	for len(s) > 0 && s[len(s)-1] == '0' {
		s = s[:len(s)-1]
	}
	if len(s) > 0 && s[len(s)-1] == '.' {
		s = s[:len(s)-1]
	}
	return s
}
