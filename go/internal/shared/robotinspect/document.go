package robotinspect

import (
	"context"
	"sort"
	"time"
)

// Schema is the document's contract. Bump it rather than changing a field's meaning:
// two documents are diffed against each other and against stored ones, so a silent
// reinterpretation is worse than a new version.
const Schema = "wendy.robot.inspection.v1"

// Legality is whether a robot will accept commands right now. The core does not decide
// this; a vendor backend maps its own state machine onto it.
type Legality string

const (
	LegalityYes     Legality = "yes"
	LegalityNo      Legality = "no"
	LegalityUnknown Legality = "unknown"
)

// VendorState is a robot's own control state, kept deliberately opaque. Vendor state
// machines are numerous, renumbered between firmware versions, and documented
// inconsistently — on one G1 the published guidance and the shipped bridge disagree
// about which state accepts low-level commands. So the core stores Raw verbatim, prints
// it under the backend's Label, and reasons only about ControlAuthority and
// CommandLegality, which every robot has in some form.
type VendorState struct {
	// Raw is whatever the backend wants to surface, such as
	// {"fsm_id": "801", "fsm_mode": "3"}. Never interpreted here.
	Raw map[string]string
	// Label is the backend's own rendering of Raw, for an operator to read.
	Label string
	// ControlAuthority names who holds the actuators: empty for nobody, a process or
	// SDK name, or "unknown".
	ControlAuthority string
	CommandLegality  Legality
	// LegalityReason explains a No or an Unknown in the backend's terms.
	LegalityReason string
}

// Document is one inspection pass. It is the product; the text rendering is one view of
// it.
type Document struct {
	Schema string
	// Device is the target as the caller resolved it, so a document can be traced to a
	// unit. VendorKind is the backend's registry key, such as "unitree-go2".
	Device     string
	VendorKind string
	StartedAt  time.Time
	FinishedAt time.Time
	// PassiveOnly records that every probe run was classified passive. It is written
	// from the plan rather than asserted by hand, so it cannot claim more than was done.
	PassiveOnly bool
	ProbesRun   []string
	Properties  []Property
	// Skipped is keyed by probe ID: what was not looked at, and why.
	Skipped map[string]Unknown
	Vendor  *VendorState
}

// Summary counts a document's verdicts. It is what the last line of the report prints
// and what a caller exits non-zero on.
type Summary struct {
	Agree        int
	Disagree     int
	Incomparable int
	Single       int
	Unknown      int
}

// Summarise counts the verdicts across every property.
func (d Document) Summarise() Summary {
	var s Summary
	for _, p := range d.Properties {
		switch p.Assess().Verdict {
		case VerdictAgree:
			s.Agree++
		case VerdictDisagree:
			s.Disagree++
		case VerdictIncomparable:
			s.Incomparable++
		case VerdictSingle:
			s.Single++
		case VerdictUnknown:
			s.Unknown++
		}
	}
	return s
}

// Findings reports whether anything needs an operator's attention. Incomparable counts:
// a declared value that was never checked against a comparable measurement is how a
// wrong number survives.
func (s Summary) Findings() int { return s.Disagree + s.Incomparable }

// Target describes what is being inspected.
type Target struct {
	Device     string
	VendorKind string
	// Want lists the property IDs the caller expects an answer for. Any that no
	// runnable probe provides are recorded as unknown, so the report distinguishes
	// "nothing looked at this" from "this robot has no such property".
	Want []string
}

// Inspect runs the passive probes that env satisfies and assembles a document. It never
// runs a probe that may actuate; a caller cannot opt out of that, which is what makes
// the read-only guarantee structural.
func Inspect(ctx context.Context, registry *Registry, env *Env, target Target) Document {
	started := time.Now().UTC()
	plan := registry.PassivePlan(env)

	doc := Document{
		Schema:      Schema,
		Device:      target.Device,
		VendorKind:  target.VendorKind,
		StartedAt:   started,
		PassiveOnly: true,
		Skipped:     map[string]Unknown{},
	}
	for id, unknown := range plan.Skipped {
		doc.Skipped[id] = unknown
	}

	byID := map[string]*Property{}
	var order []string
	for _, probe := range plan.Run {
		doc.ProbesRun = append(doc.ProbesRun, probe.ID())
		found, err := probe.Observe(ctx, env)
		if err != nil {
			doc.Skipped[probe.ID()] = NewUnknown(ReasonProbeFailed, err.Error())
		}
		for _, p := range found {
			existing, seen := byID[p.ID]
			if !seen {
				copied := p
				byID[p.ID] = &copied
				order = append(order, p.ID)
				continue
			}
			mergeProperty(existing, p)
		}
	}

	for _, id := range plan.UnprovidedProperties(target.Want) {
		if _, seen := byID[id]; seen {
			continue
		}
		unknown := NewUnknown(ReasonRequirementUnmet, "no runnable probe provides this property")
		byID[id] = &Property{ID: id, Unknown: &unknown}
		order = append(order, id)
	}

	sort.Strings(order)
	for _, id := range order {
		doc.Properties = append(doc.Properties, *byID[id])
	}
	doc.FinishedAt = time.Now().UTC()
	return doc
}

// mergeProperty folds a second probe's answer for the same property into the first.
// This is the common case, not an edge one: a declared value comes from a URDF and the
// measured one from sampling a stream, and they only become a finding once they sit in
// the same property.
func mergeProperty(into *Property, from Property) {
	into.Observations = append(into.Observations, from.Observations...)
	if len(into.Observations) > 0 {
		// An answer from any probe retires an unknown recorded by another.
		into.Unknown = nil
	} else if into.Unknown == nil {
		into.Unknown = from.Unknown
	}
	if into.Tolerance == nil {
		into.Tolerance = from.Tolerance
	}
}
