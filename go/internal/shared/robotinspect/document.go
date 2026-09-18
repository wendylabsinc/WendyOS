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
	// PassiveOnly records that every probe actually run was classified passive. It is
	// computed from the plan, so it cannot claim more than was done — it turns into
	// "nothing was commanded" in the report, which has to be earned.
	PassiveOnly bool
	ProbesRun   []string
	Properties  []Property
	// Skipped is keyed by probe ID: probes that never ran, and why.
	Skipped map[string]Unknown
	// Failed is keyed by probe ID: probes that ran and errored. Separate from Skipped
	// because a probe that returned half its answers is not one that never ran, and
	// filing it under "not run" tells an operator the opposite of what happened.
	Failed map[string]Unknown
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

	// PassiveOnly is derived from the plan, never asserted. It becomes an affirmative
	// safety claim in the operator's report and in the stored document, so a probe
	// misreporting its own class must not be able to turn into "nothing was commanded".
	passiveOnly := true
	for _, probe := range plan.Run {
		if probe.Class() != ClassPassive {
			passiveOnly = false
		}
	}

	doc := Document{
		Schema:      Schema,
		Device:      target.Device,
		VendorKind:  target.VendorKind,
		StartedAt:   started,
		PassiveOnly: passiveOnly,
		Skipped:     map[string]Unknown{},
		Failed:      map[string]Unknown{},
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
			// It ran. Whatever it returned is kept, and the failure is recorded as a
			// failure rather than as a probe that was never attempted.
			doc.Failed[probe.ID()] = NewUnknown(ReasonProbeFailed, err.Error())
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
	if into.Unknown == nil {
		into.Unknown = from.Unknown
	}
	// Only an actual observation of the thing retires an unknown. A declared value
	// does not observe anything — it restates a datasheet, a config file, or what the
	// caller asked for — so letting one clear the unknown lost the reason a
	// measurement failed: the report would say "asked for 1280x720" and give no hint
	// that nothing came back.
	if observedIn(into.Observations) {
		into.Unknown = nil
	}
	if into.Tolerance == nil {
		into.Tolerance = from.Tolerance
	}
}

// observedIn reports whether any observation is a look at the robot rather than a claim
// about it.
func observedIn(observations []Observation) bool {
	for _, o := range observations {
		if o.Kind == Measured || o.Kind == Derived {
			return true
		}
	}
	return false
}
