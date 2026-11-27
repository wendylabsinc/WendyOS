package sitepub

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T) ([]byte, Config) {
	t.Helper()
	raw, err := os.ReadFile("../../examples/geography/office.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw, Config{Tenant: "lab", Site: "synthetic-office", RunID: "publication-test", Source: world.Source{Device: "operator", App: "world", Service: "site", Stream: "publications"}, Epoch: 1, Sequence: 1, Now: time.Date(2026, 11, 9, 22, 0, 0, 0, time.UTC), TTL: time.Hour}
}
func mustBuild(t *testing.T, raw []byte, cfg Config) world.Component {
	t.Helper()
	c, e := Build(raw, cfg)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func clone(t *testing.T, c world.Component) world.Component {
	t.Helper()
	b, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	var out world.Component
	if e = json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func TestCanonicalPublicationSurvivesTransport(t *testing.T) {
	raw, cfg := fixture(t)
	var document geography.SiteConfig
	if e := json.Unmarshal(raw, &document); e != nil {
		t.Fatal(e)
	}
	document.PlacementSource = "<test>& capture \u2028 source"
	raw, e := json.Marshal(document)
	if e != nil {
		t.Fatal(e)
	}
	var pretty bytes.Buffer
	if e = json.Indent(&pretty, raw, "", "  "); e != nil {
		t.Fatal(e)
	}
	a := mustBuild(t, pretty.Bytes(), cfg)
	var unordered map[string]any
	if e = json.Unmarshal(raw, &unordered); e != nil {
		t.Fatal(e)
	}
	reordered, _ := json.Marshal(unordered)
	b := mustBuild(t, reordered, cfg)
	if a.Key.Entity != b.Key.Entity || !bytes.Equal(a.Payload, b.Payload) {
		t.Fatal("format or key ordering changed content identity")
	}
	p, e := Validate(clone(t, a))
	if e != nil {
		t.Fatal(e)
	}
	if p.SHA256 != strings.TrimPrefix(a.Key.Entity, EntityPrefix) || p.Revision != document.Revision || p.Site != document.Site || p.PackageSHA256 != document.PackageSHA256 || p.Placement != "illustrative" {
		t.Fatal(p)
	}
	var got geography.SiteConfig
	if e = json.Unmarshal(p.Document, &got); e != nil {
		t.Fatal(e)
	}
	want, _ := json.Marshal(document)
	actual, _ := json.Marshal(got)
	if !bytes.Equal(want, actual) {
		t.Fatal("publication changed document content")
	}
	p.Document[0] = 'x'
	if _, e = Validate(a); e != nil {
		t.Fatal("returned document aliases component")
	}
}
func TestPublicationBoundsAndValidity(t *testing.T) {
	raw, cfg := fixture(t)
	cfg.TTL = MaxTTL + time.Hour
	c := mustBuild(t, raw, cfg)
	if c.TTL != MaxTTL {
		t.Fatal(c.TTL)
	}
	cfg.Now = time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	c = mustBuild(t, raw, cfg)
	if c.TTL != time.Minute {
		t.Fatal(c.TTL)
	}
	for _, at := range []time.Time{time.Date(2025, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)} {
		cfg.Now = at
		if _, e := Build(raw, cfg); e == nil {
			t.Fatal("outside document interval accepted")
		}
	}
	_, cfg = fixture(t)
	for _, bad := range []func(*Config){func(c *Config) { c.Site = "wrong" }, func(c *Config) { c.Tenant = "" }, func(c *Config) { c.RunID = "" }, func(c *Config) { c.Epoch = 0 }, func(c *Config) { c.Sequence = 0 }, func(c *Config) { c.TTL = 0 }, func(c *Config) { c.Now = time.Time{} }, func(c *Config) { c.Source.Service = "" }} {
		x := cfg
		bad(&x)
		if _, e := Build(raw, x); e == nil {
			t.Fatal("invalid config accepted")
		}
	}
	if _, e := Build(bytes.Repeat([]byte(" "), MaxDocumentBytes+1), cfg); e == nil {
		t.Fatal("oversized document accepted")
	}
}
func TestPublicationRejectsTamperedEnvelopes(t *testing.T) {
	raw, cfg := fixture(t)
	original := mustBuild(t, raw, cfg)
	cases := map[string]func(*world.Component){
		"type":              func(c *world.Component) { c.Key.Type = "other" },
		"frame":             func(c *world.Component) { c.Frame.Revision = "v2" },
		"tombstone":         func(c *world.Component) { c.Tombstone = true },
		"site":              func(c *world.Component) { c.Key.Site = "other" },
		"tenant":            func(c *world.Component) { c.Key.Tenant = "" },
		"epoch":             func(c *world.Component) { c.Version.Epoch = 0 },
		"sequence":          func(c *world.Component) { c.Version.Sequence = 0 },
		"source":            func(c *world.Component) { c.Key.Source.Device = "" },
		"oversized-source":  func(c *world.Component) { c.Key.Source.Device = strings.Repeat("x", 257) },
		"hash":              func(c *world.Component) { c.Key.Entity = EntityPrefix + strings.Repeat("a", 64) },
		"ttl-zero":          func(c *world.Component) { c.TTL = 0 },
		"ttl-limit":         func(c *world.Component) { c.TTL = MaxTTL + 1 },
		"document-deadline": func(c *world.Component) { c.Timing.SampleTime = time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC) },
		"before-document":   func(c *world.Component) { c.Timing.SampleTime = time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC) },
		"clock":             func(c *world.Component) { c.Timing.ClockDomain = "physical" },
		"unknown-clock":     func(c *world.Component) { c.Timing.ErrorKnown = false },
		"negative-age":      func(c *world.Component) { c.Timing.AgeHint = -1 },
		"run":               func(c *world.Component) { c.Provenance.Simulation.RunID = "other" },
		"scenario":          func(c *world.Component) { c.Provenance.Simulation.ScenarioVersion = "other" },
		"origin":            func(c *world.Component) { c.Provenance.Origin = world.Measured; c.Provenance.Simulation = nil },
		"schema": func(c *world.Component) {
			c.Payload = bytes.Replace(c.Payload, []byte(geography.SiteSchema), []byte("other"), 1)
		},
		"whitespace": func(c *world.Component) { c.Payload = append([]byte(" "), c.Payload...) },
		"changed-doc": func(c *world.Component) {
			c.Payload = bytes.Replace(c.Payload, []byte(`"latitude_degrees":52`), []byte(`"latitude_degrees":53`), 1)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := clone(t, original)
			change(&c)
			if _, e := Validate(c); e == nil {
				t.Fatal("tampered publication accepted")
			}
		})
	}
	c := clone(t, original)
	c.Timing.AgeHint = 2 * time.Minute
	if _, e := Validate(c); e != nil {
		t.Fatal("valid relayed age rejected", e)
	}
}
func TestSameHumanRevisionDifferentContentHasDistinctIdentity(t *testing.T) {
	raw, cfg := fixture(t)
	a := mustBuild(t, raw, cfg)
	raw = bytes.Replace(raw, []byte(`"latitude_degrees": 52`), []byte(`"latitude_degrees": 53`), 1)
	b := mustBuild(t, raw, cfg)
	pa, e := Validate(a)
	if e != nil {
		t.Fatal(e)
	}
	pb, e := Validate(b)
	if e != nil {
		t.Fatal(e)
	}
	if pa.Revision != pb.Revision || a.Key.Entity == b.Key.Entity || pa.SHA256 == pb.SHA256 {
		t.Fatal("revision labels selected or merged distinct documents")
	}
	// A refresh can advance the source version but cannot change the content
	// under the existing digest. Selecting a latest revision is outside this API.
	cfg.Epoch = 2
	cfg.Sequence = 7
	cfg.Now = cfg.Now.Add(time.Minute)
	refreshed := mustBuild(t, raw, cfg)
	if refreshed.Key.Entity != b.Key.Entity {
		t.Fatal("refresh changed content identity")
	}
}
