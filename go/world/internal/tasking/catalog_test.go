package tasking

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
)

func TestCatalogCopiesTrustedConfiguration(t *testing.T) {
	r := setup(t)
	catalog := r.s.Catalog()
	if catalog.Assignee != r.s.cfg.Assignee || catalog.Domain != r.s.cfg.Domain || len(catalog.Types) == 0 {
		t.Fatal(catalog)
	}
	want := catalog.Types[0]
	r.cfg.Catalog[want.Type] = mission.Actuate
	catalog.Types[0].Class = mission.Actuate
	catalog.Types[0].Type = "forged"
	if r.s.Catalog().Types[0] != want {
		t.Fatal("returned catalog mutated trusted configuration")
	}
}
