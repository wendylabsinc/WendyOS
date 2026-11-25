// Package sitepub validates the geographic document carried by an origin-signed
// World component. It does not verify the signature or authorize its publisher.
package sitepub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	Type             = "wendy.site.georeference.v1"
	EntityPrefix     = "site-georeference/"
	MaxDocumentBytes = 32 << 10
	MaxTTL           = 30 * 24 * time.Hour
	appVersion       = "world-site-publisher-v1"
)

// MetadataFrame keeps publication visibility independent of the coordinate
// revisions in its document. Receiving a document does not activate its frames.
func MetadataFrame() world.FrameRef {
	return world.FrameRef{ID: "site-metadata", Revision: "v1"}
}

type Config struct {
	Tenant, Site, RunID string
	Source              world.Source
	Epoch, Sequence     uint64
	Now                 time.Time
	TTL                 time.Duration
}

type Publication struct {
	SHA256        string          `json:"sha256"`
	Document      json.RawMessage `json:"document"`
	Site          string          `json:"site"`
	Revision      string          `json:"revision"`
	PackageSHA256 string          `json:"package_sha256"`
	Placement     string          `json:"placement"`
	ValidFrom     time.Time       `json:"valid_from"`
	ValidUntil    time.Time       `json:"valid_until"`
}

func document(raw []byte) ([]byte, geography.SiteConfig, error) {
	var cfg geography.SiteConfig
	if len(raw) == 0 || len(raw) > MaxDocumentBytes {
		return nil, cfg, errors.New("site document exceeds publication size bound")
	}
	if _, err := geography.DecodeSite(raw); err != nil {
		return nil, cfg, fmt.Errorf("site document: %w", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, cfg, err
	}
	// Fixed struct order, float formatting and HTML escaping survive the normal
	// json.RawMessage transport encoder. Compacting alone would not escape '<'.
	canonical, err := json.Marshal(cfg)
	if err != nil || len(canonical) > MaxDocumentBytes {
		return nil, cfg, errors.New("canonical site document exceeds publication size bound")
	}
	return canonical, cfg, nil
}

func digest(raw []byte) string {
	hash := sha256.Sum256(raw)
	return hex.EncodeToString(hash[:])
}

func textBounded(value string) bool { return len(value) > 0 && len(value) <= 256 }

// Build validates and canonicalizes the document before allocating its content
// identity. Callers still sign the component using an authorized operator grant.
func Build(raw []byte, cfg Config) (world.Component, error) {
	payload, site, err := document(raw)
	if err != nil {
		return world.Component{}, err
	}
	if !textBounded(cfg.Tenant) || cfg.Site != site.Site || !textBounded(cfg.Site) || !textBounded(cfg.RunID) || cfg.Epoch == 0 || cfg.Sequence == 0 || cfg.Now.IsZero() || cfg.TTL <= 0 {
		return world.Component{}, errors.New("invalid site publication configuration")
	}
	if cfg.Now.Before(site.ValidFrom) || !cfg.Now.Before(site.ValidUntil) {
		return world.Component{}, errors.New("site document is not currently valid")
	}
	ttl := min(cfg.TTL, MaxTTL, site.ValidUntil.Sub(cfg.Now))
	c := world.Component{
		Key:     world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: EntityPrefix + digest(payload), Type: Type, Source: cfg.Source},
		Version: world.Version{Epoch: cfg.Epoch, Sequence: cfg.Sequence},
		Frame:   MetadataFrame(),
		Timing:  world.Timing{SampleTime: cfg.Now.Round(0), ClockDomain: "simulation:" + cfg.RunID, ErrorKnown: true},
		TTL:     ttl,
		Provenance: world.Provenance{Origin: world.Simulated, AppVersion: appVersion,
			Simulation: &world.SimulationContext{RunID: cfg.RunID, ScenarioVersion: site.Revision, ClockDomain: "simulation:" + cfg.RunID}},
		Payload: payload,
	}
	if _, err := Validate(c); err != nil {
		return world.Component{}, err
	}
	return c, nil
}

// Validate checks immutable publication content, not current visibility or
// authority. The node must verify origin signatures, operator scope, freshness,
// revocations and expiry before exposing this document as an accepted revision.
func Validate(c world.Component) (*Publication, error) {
	if c.Key.Type != Type || c.Frame != MetadataFrame() || c.Tombstone || !textBounded(c.Key.Tenant) || !textBounded(c.Key.Site) || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.TTL <= 0 || c.TTL > MaxTTL {
		return nil, errors.New("invalid site publication envelope")
	}
	if err := c.Key.Source.Validate(); err != nil {
		return nil, err
	}
	if !textBounded(c.Key.Source.Device) || !textBounded(c.Key.Source.App) || !textBounded(c.Key.Source.Service) || !textBounded(c.Key.Source.Stream) {
		return nil, errors.New("site publication source exceeds bound")
	}
	if err := c.Timing.Validate(); err != nil {
		return nil, err
	}
	if !c.Timing.ErrorKnown {
		return nil, errors.New("site publication requires a known clock bound")
	}
	if err := c.Provenance.Validate(); err != nil {
		return nil, err
	}
	sim := c.Provenance.Simulation
	if c.Provenance.Origin != world.Simulated || sim == nil || !textBounded(sim.RunID) || c.Timing.ClockDomain != "simulation:"+sim.RunID || sim.ClockDomain != c.Timing.ClockDomain {
		return nil, errors.New("site publication must retain its simulation clock domain")
	}
	payload, cfg, err := document(c.Payload)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(c.Payload, payload) {
		return nil, errors.New("site publication payload is not canonical")
	}
	hash := digest(payload)
	if c.Key.Entity != EntityPrefix+hash || cfg.Site != c.Key.Site || sim.ScenarioVersion != cfg.Revision {
		return nil, errors.New("site publication digest, site or revision mismatch")
	}
	if c.Timing.SampleTime.Before(cfg.ValidFrom) || !c.Timing.SampleTime.Before(cfg.ValidUntil) || c.TTL > cfg.ValidUntil.Sub(c.Timing.SampleTime) {
		return nil, errors.New("site publication exceeds document validity")
	}
	return &Publication{SHA256: hash, Document: payload, Site: cfg.Site, Revision: cfg.Revision, PackageSHA256: cfg.PackageSHA256, Placement: cfg.Placement, ValidFrom: cfg.ValidFrom, ValidUntil: cfg.ValidUntil}, nil
}
