package robotinspect

import (
	"fmt"
	"sort"
	"strings"
)

// Render writes the operator's view of a document. The document is the product and this
// is one projection of it, so nothing is computed here that a --json consumer would not
// also see: the verdicts, details and reasons all come from the document itself.
//
// Every value is printed with its kind, its qualifiers and where it came from. A bare
// number never appears, because a bare number is what let a declared horizontal angle
// pass for a measured vertical one.
func Render(d Document) string {
	var b strings.Builder

	header := d.Device
	if d.VendorKind != "" {
		header = strings.TrimSpace(header + " (" + d.VendorKind + ")")
	}
	if header != "" {
		fmt.Fprintf(&b, "%s\n\n", header)
	}

	for _, section := range sectionsOf(d.Properties) {
		fmt.Fprintf(&b, "%s\n", section.name)
		for _, p := range section.properties {
			renderProperty(&b, p)
		}
		b.WriteByte('\n')
	}

	renderProbeNotes(&b, "not run", d.Skipped)
	renderProbeNotes(&b, "failed", d.Failed)

	renderSummary(&b, d)
	return b.String()
}

// renderProbeNotes lists probes under a heading that says what actually happened to
// them. A probe that ran and errored appearing under "not run" is worse than silence.
func renderProbeNotes(b *strings.Builder, heading string, notes map[string]Unknown) {
	if len(notes) == 0 {
		return
	}
	b.WriteString(heading + "\n")
	ids := make([]string, 0, len(notes))
	for id := range notes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		note := notes[id]
		line := fmt.Sprintf("%s — %s", id, note.Reason)
		if note.Detail != "" {
			line = fmt.Sprintf("%s — %s: %s", id, note.Reason, note.Detail)
		}
		fmt.Fprintf(b, "%-10s %s\n", "", line)
	}
	b.WriteByte('\n')
}

func renderProperty(b *strings.Builder, p Property) {
	assessment := p.Assess()
	fmt.Fprintf(b, "%-10s %s\n", "", p.ID)

	for _, o := range p.Observations {
		detail := fmt.Sprintf("%s: %s", o.Source.Probe, o.Source.Origin)
		if o.Sampling != nil {
			detail = fmt.Sprintf("%s, %d samples over %s", detail, o.Sampling.Samples, o.Sampling.Window)
		}
		if conditions := o.conditionKey(); conditions != "" {
			detail = fmt.Sprintf("%s, %s", detail, conditions)
		}
		fmt.Fprintf(b, "%-10s   %-9s %-26s (%s)\n", "", o.Kind, o.Display(), detail)
	}

	switch assessment.Verdict {
	case VerdictDisagree, VerdictIncomparable:
		fmt.Fprintf(b, "%-10s   %s %s: %s\n", "", warn, assessment.Verdict, assessment.Detail)
	case VerdictUnknown:
		line := "UNKNOWN"
		if assessment.Detail != "" {
			line += " — " + assessment.Detail
		}
		fmt.Fprintf(b, "%-10s   %s %s\n", "", warn, line)
	case VerdictSingle:
		fmt.Fprintf(b, "%-10s   %s\n", "", assessment.Detail)
	}
}

func renderSummary(b *strings.Builder, d Document) {
	s := d.Summarise()
	parts := []string{
		fmt.Sprintf("%s, %s, %s",
			plural(s.Disagree, "disagreement", "disagreements"),
			plural(s.Incomparable, "incomparable", "incomparable"),
			plural(s.Unknown, "unknown", "unknowns")),
	}
	if s.Agree > 0 {
		parts = append(parts, fmt.Sprintf("%d confirmed", s.Agree))
	}
	if s.Single > 0 {
		parts = append(parts, fmt.Sprintf("%d unchecked", s.Single))
	}
	b.WriteString(strings.Join(parts, ", ") + ".")

	// Earned from the plan, not asserted: only passive probes can be scheduled.
	if d.PassiveOnly {
		b.WriteString("  Nothing was commanded.")
	}
	b.WriteByte('\n')
}

// warn marks the rows an operator has to act on.
const warn = "!"

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// section groups the properties that share a first dotted segment, so a report reads as
// "cameras", "joints", "ros2" rather than as a flat list of identifiers.
type section struct {
	name       string
	properties []Property
}

// sectionsOf groups properties by their first segment, keeping both the sections and the
// properties inside them in the document's own sorted order.
func sectionsOf(properties []Property) []section {
	var sections []section
	index := map[string]int{}
	for _, p := range properties {
		name := p.ID
		if dot := strings.IndexByte(p.ID, '.'); dot > 0 {
			name = p.ID[:dot]
		}
		at, seen := index[name]
		if !seen {
			index[name] = len(sections)
			sections = append(sections, section{name: name, properties: []Property{p}})
			continue
		}
		sections[at].properties = append(sections[at].properties, p)
	}
	return sections
}
