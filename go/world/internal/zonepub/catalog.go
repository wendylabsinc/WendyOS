// Package zonepub defines signed, content-addressed zone catalogs. Geometry is
// descriptive metadata and never grants motion or task authority.
package zonepub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Type = "wendy.zone.catalog.v1"
const Prefix = "zone-catalog/"
const MaxBytes = 16 << 10
const MaxReplyBytes = 1 << 20

type Catalog struct {
	SchemaVersion string          `json:"schema_version"`
	Name          string          `json:"name"`
	Revision      string          `json:"revision"`
	Site          string          `json:"site"`
	RunID         string          `json:"run_id"`
	Frame         world.FrameRef  `json:"frame"`
	ValidFrom     time.Time       `json:"valid_from"`
	ValidUntil    time.Time       `json:"valid_until"`
	Zones         []presence.Zone `json:"zones"`
}

type Publication struct {
	SHA256  string  `json:"sha256"`
	Catalog Catalog `json:"catalog"`
}

type Reply struct {
	Records []authority.SignedRecord `json:"records"`
}
type Selection struct {
	Publication *Publication           `json:"publication"`
	Record      authority.SignedRecord `json:"signed_record"`
	CheckedAt   time.Time              `json:"checked_at"`
	State       string                 `json:"state"`
}

func bounded(s string) bool {
	return s != "" && len(s) <= 120 && strings.TrimSpace(s) == s && utf8.ValidString(s)
}
func ValidDigest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func Digest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }

func Decode(raw []byte) (Catalog, []byte, error) {
	var c Catalog
	if err := authority.DecodeStrictLimit(raw, &c, MaxBytes); err != nil {
		return c, nil, err
	}
	if c.SchemaVersion != Type || !bounded(c.Name) || !bounded(c.Revision) || !bounded(c.Site) || !bounded(c.RunID) || c.Frame.Validate() != nil || c.ValidFrom.IsZero() || !c.ValidFrom.Before(c.ValidUntil) || c.ValidUntil.Sub(c.ValidFrom) > 30*24*time.Hour {
		return c, nil, errors.New("invalid zone catalog scope or validity")
	}
	// Fixed Go arrays otherwise silently accept missing/null/short corners.
	var shape struct {
		Zones []map[string]json.RawMessage `json:"zones"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		return c, nil, err
	}
	for _, z := range shape.Zones {
		for _, key := range []string{"min_xz", "max_xz"} {
			var pair []*float64
			if json.Unmarshal(z[key], &pair) != nil || len(pair) != 2 || pair[0] == nil || pair[1] == nil {
				return c, nil, errors.New("zone corners require two explicit finite coordinates")
			}
		}
	}
	if _, err := presence.New(c.Zones, c.RunID, "simulation:"+c.RunID, c.Frame); err != nil {
		return c, nil, err
	}
	sort.Slice(c.Zones, func(i, j int) bool { return c.Zones[i].ID < c.Zones[j].ID })
	canonical, err := json.Marshal(c)
	if err != nil || len(canonical) > MaxBytes {
		return c, nil, errors.New("zone catalog exceeds bound")
	}
	return c, canonical, nil
}

func Build(raw []byte, cfg sitepub.Config) (world.Component, error) {
	d, canonical, err := Decode(raw)
	if err != nil {
		return world.Component{}, err
	}
	if cfg.Site != d.Site || cfg.RunID != d.RunID || !bounded(cfg.Tenant) || cfg.Source.Validate() != nil || cfg.Epoch == 0 || cfg.Sequence == 0 || cfg.Now.Before(d.ValidFrom) || !cfg.Now.Before(d.ValidUntil) || cfg.TTL <= 0 || cfg.TTL > authority.DefaultRecordLifetime {
		return world.Component{}, errors.New("catalog publication scope or validity differs")
	}
	c := world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: Prefix + Digest(canonical), Type: Type, Source: cfg.Source}, Version: world.Version{Epoch: cfg.Epoch, Sequence: cfg.Sequence}, Frame: sitepub.MetadataFrame(), Timing: world.Timing{SampleTime: cfg.Now.Round(0), ClockDomain: "simulation:" + d.RunID, ErrorKnown: true}, TTL: min(cfg.TTL, d.ValidUntil.Sub(cfg.Now)), Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "world-zone-v1", Simulation: &world.SimulationContext{RunID: d.RunID, ScenarioVersion: d.Revision, ClockDomain: "simulation:" + d.RunID}}, Payload: canonical}
	_, err = Validate(c)
	return c, err
}

// Delete creates a new source-owned tombstone for an exact revision. Its empty
// payload follows the existing world register contract and retains the key.
func Delete(c world.Component, epoch uint64, now time.Time, ttl time.Duration) (world.Component, error) {
	if _, err := Validate(c); err != nil {
		return world.Component{}, err
	}
	c.Version = world.Version{Epoch: epoch, Sequence: 1}
	c.Timing.SampleTime = now.Round(0)
	c.Timing.AgeHint = 0
	c.TTL = ttl
	c.Tombstone = true
	c.Payload = nil
	_, err := Validate(c)
	return c, err
}

func Validate(c world.Component) (*Publication, error) {
	if c.Key.Type != Type || !strings.HasPrefix(c.Key.Entity, Prefix) || !ValidDigest(strings.TrimPrefix(c.Key.Entity, Prefix)) || !bounded(c.Key.Tenant) || !bounded(c.Key.Site) || c.Key.Source.Validate() != nil || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Frame != sitepub.MetadataFrame() || c.TTL <= 0 || c.TTL > authority.DefaultRecordLifetime || c.Timing.Validate() != nil || !c.Timing.ErrorKnown || c.Provenance.Validate() != nil {
		return nil, errors.New("invalid zone publication envelope")
	}
	sim := c.Provenance.Simulation
	if c.Provenance.Origin != world.Simulated || sim == nil || !bounded(sim.RunID) || c.Timing.ClockDomain != "simulation:"+sim.RunID || sim.ClockDomain != c.Timing.ClockDomain {
		return nil, errors.New("zone publication requires explicit simulation provenance")
	}
	if c.Tombstone {
		if len(c.Payload) != 0 {
			return nil, errors.New("zone tombstone carries payload")
		}
		return nil, nil
	}
	d, canonical, err := Decode(c.Payload)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, c.Payload) || c.Key.Entity != Prefix+Digest(canonical) || d.Site != c.Key.Site || d.RunID != sim.RunID || d.Revision != sim.ScenarioVersion || c.Timing.SampleTime.Before(d.ValidFrom) || !c.Timing.SampleTime.Before(d.ValidUntil) || c.Timing.SampleTime.Add(c.TTL).After(d.ValidUntil) {
		return nil, errors.New("zone publication content or scope differs")
	}
	return &Publication{Digest(canonical), d}, nil
}

func Verify(record authority.SignedRecord, verifier *authority.Verifier, now time.Time) (*Publication, error) {
	id, err := verifier.VerifyRecord(record)
	if err != nil {
		return nil, err
	}
	if id.Kind != "operator" || !id.Allows("site-publish") {
		return nil, authority.ErrDenied
	}
	p, err := Validate(record.Component)
	if err != nil {
		return nil, err
	}
	c := record.Component
	age := now.Sub(c.Timing.SampleTime)
	if age < 0 || c.Timing.ErrorBound >= c.TTL || age >= c.TTL-c.Timing.ErrorBound || c.Timing.AgeHint >= c.TTL {
		return nil, errors.New("zone publication expired or future")
	}
	if p != nil && (now.Before(p.Catalog.ValidFrom) || !now.Before(p.Catalog.ValidUntil)) {
		return nil, errors.New("zone catalog expired or future")
	}
	return p, nil
}
