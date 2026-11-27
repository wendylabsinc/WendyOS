package annotations

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func testNote(t *testing.T) world.Component {
	t.Helper()
	now := time.Date(2026, 11, 10, 12, 0, 0, 120000000, time.UTC)
	c, err := Build(Note{ID: "12345678-1234-4123-8123-123456789abc", AuthorPrincipal: "operator", Text: "Inspect north door", PositionMM: [3]int64{100, 0, -200}, AuthoredAtUnixMS: now.UnixMilli(), ExpiresAtUnixMS: now.Add(time.Hour).UnixMilli()}, Config{Tenant: "t", Site: "s", RunID: "run", Source: world.Source{Device: "d", App: "a", Service: "s", Stream: "notes"}, Frame: world.FrameRef{ID: "floor", Revision: "v1"}, Epoch: 1, Sequence: 1})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExactAnnotationSchemaAndProvenance(t *testing.T) {
	for name, change := range map[string]func(*world.Component){
		"unknown field": func(c *world.Component) {
			c.Payload = append(c.Payload[:len(c.Payload)-1], []byte(`,"zone":"clear"}`)...)
		},
		"duplicate field": func(c *world.Component) {
			c.Payload = append(c.Payload[:len(c.Payload)-1], []byte(`,"text":"Inspect north door"}`)...)
		},
		"missing coordinate": func(c *world.Component) {
			c.Payload = json.RawMessage(strings.Replace(string(c.Payload), "[100,0,-200]", "[100,0]", 1))
		},
		"extra coordinate": func(c *world.Component) {
			c.Payload = json.RawMessage(strings.Replace(string(c.Payload), "[100,0,-200]", "[100,0,-200,0]", 1))
		},
		"measured": func(c *world.Component) {
			c.Provenance = world.Provenance{Origin: world.Measured, AppVersion: AppVersion}
		},
		"model":                func(c *world.Component) { c.Provenance.ModelID = "invented" },
		"wrong run":            func(c *world.Component) { c.Provenance.Simulation.RunID = "other" },
		"wrong frame revision": func(c *world.Component) { c.Frame.Revision = "other" },
		"tombstone":            func(c *world.Component) { c.Tombstone = true; c.Payload = nil },
		"renewed time":         func(c *world.Component) { c.Timing.SampleTime = c.Timing.SampleTime.Add(time.Second) },
		"renewed TTL":          func(c *world.Component) { c.TTL += time.Second },
		"unknown time":         func(c *world.Component) { c.Timing.ErrorKnown = false },
		"claimed exact time":   func(c *world.Component) { c.Timing.ErrorBound = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			c := testNote(t)
			change(&c)
			if _, err := Validate(c); err == nil {
				t.Fatal("invalid note accepted")
			}
		})
	}
	c := testNote(t)
	if !c.Provenance.Synthetic() {
		t.Fatal("authored simulation note lost synthetic domain")
	}
	c.Timing.AgeHint = time.Second * 5
	if _, err := Validate(c); err != nil {
		t.Fatal("relay age hint must remain permitted", err)
	}
}

func TestAnnotationBounds(t *testing.T) {
	c := testNote(t)
	n, _ := Validate(c)
	for name, change := range map[string]func(*Note){
		"uppercase uuid": func(n *Note) { n.ID = strings.ToUpper(n.ID) },
		"blank":          func(n *Note) { n.Text = " \n\t" },
		"utf8 bytes":     func(n *Note) { n.Text = strings.Repeat("é", 1025) },
		"coordinate":     func(n *Note) { n.PositionMM[2] = MaxPositionMM + 1 },
		"ttl":            func(n *Note) { n.ExpiresAtUnixMS = n.AuthoredAtUnixMS + MaxTTL.Milliseconds() + 1 },
		"zero ttl":       func(n *Note) { n.ExpiresAtUnixMS = n.AuthoredAtUnixMS },
	} {
		t.Run(name, func(t *testing.T) {
			note := n
			change(&note)
			payload, _ := json.Marshal(note)
			candidate := c
			candidate.Payload = payload
			if _, err := Validate(candidate); err == nil {
				t.Fatal("out-of-bounds note accepted")
			}
		})
	}
}
