package robotinspect

import (
	"encoding/json"
	"time"
)

// The wire types below are the document's public shape. They are spelled out rather
// than derived from the in-memory structs on purpose: Quantity keeps its fields
// unexported to stop an under-qualified value being constructed, and the JSON is a
// contract that outlives any refactor of the types behind it.
//
// Every observation carries its qualifiers inline — no consumer should have to guess
// which axis a number is on — and each property carries the verdict the core computed,
// so a caller never reimplements Assess and reaches a different conclusion.

type wireDocument struct {
	Schema      string             `json:"schema"`
	Device      string             `json:"device,omitempty"`
	VendorKind  string             `json:"vendorKind,omitempty"`
	StartedAt   *time.Time         `json:"startedAt,omitempty"`
	FinishedAt  *time.Time         `json:"finishedAt,omitempty"`
	PassiveOnly bool               `json:"passiveOnly"`
	ProbesRun   []string           `json:"probesRun,omitempty"`
	Summary     wireSummary        `json:"summary"`
	Properties  []wireProperty     `json:"properties"`
	Skipped     map[string]Unknown `json:"skipped,omitempty"`
	Vendor      *wireVendorState   `json:"vendor,omitempty"`
}

type wireSummary struct {
	Agree        int `json:"agree"`
	Disagree     int `json:"disagree"`
	Incomparable int `json:"incomparable"`
	Single       int `json:"single"`
	Unknown      int `json:"unknown"`
	Findings     int `json:"findings"`
}

type wireProperty struct {
	ID           string            `json:"id"`
	Verdict      Verdict           `json:"verdict"`
	Detail       string            `json:"detail,omitempty"`
	Tolerance    *Tolerance        `json:"tolerance,omitempty"`
	Observations []wireObservation `json:"observations,omitempty"`
	Unknown      *Unknown          `json:"unknown,omitempty"`
}

type wireObservation struct {
	Kind Kind `json:"kind"`
	// Exactly one of Value or Text is present. A consumer switches on which.
	Value      *float64          `json:"value,omitempty"`
	Text       string            `json:"text,omitempty"`
	Unit       string            `json:"unit,omitempty"`
	Axis       string            `json:"axis,omitempty"`
	Frame      string            `json:"frame,omitempty"`
	Source     Source            `json:"source"`
	Conditions map[string]string `json:"conditions,omitempty"`
	Sampling   *wireSampling     `json:"sampling,omitempty"`
	ObservedAt *time.Time        `json:"observedAt,omitempty"`
}

type wireSampling struct {
	WindowMS int64 `json:"windowMs"`
	Samples  int   `json:"samples"`
}

type wireVendorState struct {
	Raw              map[string]string `json:"raw,omitempty"`
	Label            string            `json:"label,omitempty"`
	ControlAuthority string            `json:"controlAuthority,omitempty"`
	CommandLegality  Legality          `json:"commandLegality,omitempty"`
	LegalityReason   string            `json:"legalityReason,omitempty"`
}

// MarshalJSON renders the document, timestamps included.
func (d Document) MarshalJSON() ([]byte, error) { return json.Marshal(d.wire(false)) }

// CanonicalJSON renders the document without wall-clock fields, so two passes — the
// same robot a week apart, or two units off the same line — diff on substance instead
// of on when they ran. This is the form to store and to compare against.
func (d Document) CanonicalJSON() ([]byte, error) {
	return json.MarshalIndent(d.wire(true), "", "  ")
}

func (d Document) wire(canonical bool) wireDocument {
	summary := d.Summarise()
	out := wireDocument{
		Schema:      d.Schema,
		Device:      d.Device,
		VendorKind:  d.VendorKind,
		PassiveOnly: d.PassiveOnly,
		ProbesRun:   d.ProbesRun,
		Summary: wireSummary{
			Agree:        summary.Agree,
			Disagree:     summary.Disagree,
			Incomparable: summary.Incomparable,
			Single:       summary.Single,
			Unknown:      summary.Unknown,
			Findings:     summary.Findings(),
		},
		Properties: make([]wireProperty, 0, len(d.Properties)),
		Skipped:    d.Skipped,
	}
	if !canonical {
		started, finished := d.StartedAt, d.FinishedAt
		if !started.IsZero() {
			out.StartedAt = &started
		}
		if !finished.IsZero() {
			out.FinishedAt = &finished
		}
	}
	for _, p := range d.Properties {
		assessment := p.Assess()
		wp := wireProperty{
			ID:        p.ID,
			Verdict:   assessment.Verdict,
			Detail:    assessment.Detail,
			Tolerance: p.Tolerance,
			Unknown:   p.Unknown,
		}
		for _, o := range p.Observations {
			wp.Observations = append(wp.Observations, o.wire(canonical))
		}
		out.Properties = append(out.Properties, wp)
	}
	if d.Vendor != nil {
		out.Vendor = &wireVendorState{
			Raw:              d.Vendor.Raw,
			Label:            d.Vendor.Label,
			ControlAuthority: d.Vendor.ControlAuthority,
			CommandLegality:  d.Vendor.CommandLegality,
			LegalityReason:   d.Vendor.LegalityReason,
		}
	}
	return out
}

func (o Observation) wire(canonical bool) wireObservation {
	out := wireObservation{
		Kind:       o.Kind,
		Source:     o.Source,
		Conditions: o.Conditions,
	}
	if o.IsText() {
		out.Text = o.Text
	} else {
		value := o.Quantity.value
		out.Value = &value
		out.Unit = o.Quantity.unit.symbol
		out.Axis = o.Quantity.axis
		out.Frame = o.Quantity.frame
	}
	if o.Sampling != nil {
		out.Sampling = &wireSampling{
			WindowMS: o.Sampling.Window.Milliseconds(),
			Samples:  o.Sampling.Samples,
		}
	}
	if !canonical && !o.ObservedAt.IsZero() {
		at := o.ObservedAt
		out.ObservedAt = &at
	}
	return out
}
