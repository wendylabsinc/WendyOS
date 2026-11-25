package zonepub

import (
	"bytes"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) ([]byte, sitepub.Config) {
	t.Helper()
	now := time.Now().Round(0)
	d := Catalog{Type, "Office", "v1", "office", "run", world.FrameRef{ID: "floor", Revision: "1"}, now.Add(-time.Minute), now.Add(time.Hour), []presence.Zone{{ID: "room", Label: "Room", MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{2, 2}, DwellSeconds: 2}}}
	raw, e := json.Marshal(d)
	if e != nil {
		t.Fatal(e)
	}
	return raw, sitepub.Config{Tenant: "tenant", Site: "office", RunID: "run", Source: world.Source{Device: "a", App: "world", Service: "zones", Stream: "catalog"}, Epoch: 1, Sequence: 1, Now: now, TTL: time.Hour}
}
func TestCatalogStrictGeometryAndImmutableDigest(t *testing.T) {
	raw, cfg := fixture(t)
	c, e := Build(raw, cfg)
	if e != nil {
		t.Fatal(e)
	}
	p, e := Validate(c)
	if e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{`null`, `[null,0]`, `[0,null]`, `[0]`, `[0,0,1]`, `["0",0]`} {
		broken := bytes.Replace(raw, []byte(`"min_xz":[0,0]`), []byte(`"min_xz":`+bad), 1)
		if _, _, e := Decode(broken); e == nil {
			t.Fatalf("accepted corner %s", bad)
		}
	}
	for _, mutate := range []func(*world.Component){func(c *world.Component) { c.Payload = bytes.Replace(c.Payload, []byte(`"Room"`), []byte(`"Other"`), 1) }, func(c *world.Component) { c.Key.Entity = Prefix + strings.Repeat("a", 64) }, func(c *world.Component) { c.Key.Site = "elsewhere" }, func(c *world.Component) { c.Provenance.Simulation.RunID = "other" }, func(c *world.Component) { c.Frame.Revision = "2" }} {
		copy := c
		copy.Payload = bytes.Clone(c.Payload)
		sim := *c.Provenance.Simulation
		copy.Provenance.Simulation = &sim
		mutate(&copy)
		if _, e := Validate(copy); e == nil {
			t.Fatal("accepted changed binding")
		}
	}
	changed := bytes.Replace(raw, []byte(`"max_xz":[2,2]`), []byte(`"max_xz":[3,2]`), 1)
	next, e := Build(changed, cfg)
	if e != nil {
		t.Fatal(e)
	}
	np, _ := Validate(next)
	if np.Catalog.Revision != p.Catalog.Revision || np.SHA256 == p.SHA256 {
		t.Fatal("label selected revision")
	}
	deleted, e := Delete(c, 2, cfg.Now, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	if !deleted.Tombstone || len(deleted.Payload) != 0 || deleted.Key != c.Key {
		t.Fatal("invalid tombstone")
	}
	if p, e := Validate(deleted); e != nil || p != nil {
		t.Fatal(e)
	}
}
func TestInvalidCatalogCannotAllocateValidPublication(t *testing.T) {
	raw, cfg := fixture(t)
	for _, mutate := range []func(*Catalog){func(c *Catalog) { c.Zones = nil }, func(c *Catalog) { c.ValidUntil = c.ValidFrom }, func(c *Catalog) { c.Frame.Revision = "" }, func(c *Catalog) { c.Zones = append(c.Zones, c.Zones[0]) }, func(c *Catalog) { c.Zones[0].MaxXZ = c.Zones[0].MinXZ }, func(c *Catalog) { c.Zones[0].DwellSeconds = 0 }, func(c *Catalog) { c.Site = "other" }, func(c *Catalog) { c.RunID = "other" }} {
		var d Catalog
		json.Unmarshal(raw, &d)
		mutate(&d)
		b, _ := json.Marshal(d)
		if _, e := Build(b, cfg); e == nil {
			t.Fatal("invalid catalog accepted")
		}
	}
}
