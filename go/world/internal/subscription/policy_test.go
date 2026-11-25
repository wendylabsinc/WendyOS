package subscription

import (
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"testing"
)

func testPolicy() Policy {
	return Policy{Version: 1, Tenant: "tenant", Site: "office", Selection: selection.Selection{Types: []string{"gas", "pose"}}, RequiredTypes: []string{sitepub.Type}}
}
func TestCanonicalPolicyAndMetadata(t *testing.T) {
	p := testPolicy()
	p.Selection.Types = []string{"pose", "gas"}
	n, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if n.Selection.Types[0] != "gas" || p.Selection.Types[0] != "pose" {
		t.Fatal("input mutated")
	}
	canonical := testPolicy()
	if n.Digest() != canonical.Digest() {
		t.Fatal("noncanonical digest")
	}
	key := world.Key{Tenant: "tenant", Site: "office", Type: "gas"}
	if !n.Allows(key) {
		t.Fatal("selected key")
	}
	key.Type = "audio"
	if n.Allows(key) {
		t.Fatal("unselected key")
	}
	key.Type = sitepub.Type
	if !n.Allows(key) {
		t.Fatal("required metadata")
	}
	key.Site = "other"
	if n.Allows(key) {
		t.Fatal("scope broadened")
	}
	clone := n.Clone()
	clone.Selection.Types[0] = "other"
	clone.RequiredTypes[0] = "other"
	if n.Selection.Types[0] != "gas" || n.RequiredTypes[0] != sitepub.Type {
		t.Fatal("mutable policy alias")
	}
}
func TestPolicyRejectsAmbiguousOrBroadenedScope(t *testing.T) {
	for _, change := range []func(*Policy){func(p *Policy) { p.Version = 2 }, func(p *Policy) { p.Tenant = "" }, func(p *Policy) { p.Selection = selection.Selection{} }, func(p *Policy) { p.Selection.Frames = []world.FrameRef{{ID: "floor", Revision: "1"}} }, func(p *Policy) { p.RequiredTypes = nil }, func(p *Policy) { p.RequiredTypes = append(p.RequiredTypes, "other") }, func(p *Policy) { p.Selection.Types = []string{"gas", "gas"} }} {
		p := testPolicy()
		change(&p)
		if _, err := p.Normalize(); err == nil {
			t.Fatalf("accepted %+v", p)
		}
	}
}

func TestRegionNeedsUnknownPolicy(t *testing.T) {
	p := testPolicy()
	p.Selection.Types = []string{"wendy.hazard.v1"}
	p.Selection.Region = &selection.Region{Frame: world.FrameRef{ID: "floor", Revision: "1"}, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{2, 2}}
	if _, err := p.Normalize(); err == nil {
		t.Fatal("regional storage needs explicit unknown-location policy")
	}
}

func TestRegionKeyInterest(t *testing.T) {
	p := testPolicy()
	p.Selection.Types = []string{"wendy.hazard.v1"}
	p.Selection.Region = &selection.Region{Frame: world.FrameRef{ID: "floor", Revision: "1"}, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{2, 2}}
	p.UnknownLocation = "retain"
	n, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	c := world.Component{Key: world.Key{Tenant: "tenant", Site: "office", Type: "wendy.hazard.v1"}, Frame: p.Selection.Region.Frame, Payload: []byte(`{"position_m":[20,0,20],"uncertainty_m":0}`)}
	if !n.Allows(c.Key) || n.Retains(c) {
		t.Fatal("outside movement not available for fencing")
	}
	clone := n.Clone()
	clone.Selection.Region.MaxXZ[0] = 3
	if n.Selection.Region.MaxXZ[0] != 2 {
		t.Fatal("region alias")
	}
	p.Selection.Types = []string{"wendy.annotation.v2"}
	if _, err = p.Normalize(); err == nil {
		t.Fatal("unsupported annotation history accepted")
	}
}
