package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
)

func TestCommissionedContextRequiresActualAdapterRunAndPackageBeforeEvaluation(t *testing.T) {
	for _, field := range []string{"match", "run", "package", "site", "frame", "provenance", "absent"} {
		t.Run(field, func(t *testing.T) {
			c, a, g := controller(t)
			bound := &missioncontrol.SignedContext{Context: missioncontrol.Context{
				RunID: "commissioned", Site: "synthetic-office", PackageSHA256: strings.Repeat("a", 64), Frame: a.state.Frame,
			}}
			g.records[0].Spec.ControlContext, g.decision.Record.Spec.ControlContext = bound, bound
			a.state.RunID, a.state.Site = bound.Context.RunID, bound.Context.Site
			a.state.Provenance = StateProvenance{Mode: "simulated", PackageSHA256: bound.Context.PackageSHA256}
			switch field {
			case "run":
				a.state.RunID = "different"
			case "package":
				a.state.Provenance.PackageSHA256 = strings.Repeat("b", 64)
			case "site":
				a.state.Site = "different"
			case "frame":
				a.state.Frame.Revision = "different"
			case "provenance":
				a.state.Provenance.Mode = "measured"
			case "absent":
				a.state.RunID = ""
				a.state.Provenance = StateProvenance{}
			}
			_, err := c.Tick(context.Background())
			if field == "match" {
				if err != nil || a.commands != 1 || g.evaluations != 1 {
					t.Fatalf("matching context did not execute: %v", err)
				}
			} else if err == nil || a.commands != 0 || g.evaluations != 0 {
				t.Fatalf("mismatched context evaluated or moved: %v", err)
			}
		})
	}
}
