package geography

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const SiteSchema = "wendy.world.georeference.v1"
const SiteSchemaV2 = "wendy.world.georeference.v2"

// SiteConfig is an explicit placement assertion, not a signed publication or
// authority to execute hardware. Its revision is independent of geometry.
type SiteConfig struct {
	SchemaVersion   string                 `json:"schema_version"`
	Site            string                 `json:"site"`
	Revision        string                 `json:"revision"`
	PackageSHA256   string                 `json:"package_sha256"`
	Root            world.FrameRef         `json:"root"`
	Anchor          world.GeographicAnchor `json:"anchor"`
	Axes            [3]string              `json:"axes"`
	Placement       string                 `json:"placement"`
	PlacementSource string                 `json:"placement_source"`
	ValidFrom       time.Time              `json:"valid_from"`
	ValidUntil      time.Time              `json:"valid_until"`
	Frames          []world.LocalFrame     `json:"frames"`
	RootBasisError  *world.AngularError    `json:"root_basis_error,omitempty"`
}
type Site struct {
	config  SiteConfig
	graph   *Graph
	tangent *Tangent
}
type Request struct {
	From              world.FrameRef `json:"from"`
	To                world.FrameRef `json:"to"`
	Position          [3]float64     `json:"position_m"`
	At                time.Time      `json:"at"`
	UncertaintyMetres float64        `json:"uncertainty_metres"`
}
type GeoRequest struct {
	Coordinate        Coordinate     `json:"geographic"`
	To                world.FrameRef `json:"to"`
	At                time.Time      `json:"at"`
	UncertaintyMetres float64        `json:"uncertainty_metres"`
}
type Result struct {
	Site                    string           `json:"site"`
	GeoreferenceRevision    string           `json:"georeference_revision"`
	From                    world.FrameRef   `json:"from"`
	To                      world.FrameRef   `json:"to"`
	Position                [3]float64       `json:"position_m"`
	Geographic              Coordinate       `json:"geographic"`
	UncertaintyMetres       float64          `json:"uncertainty_metres"`
	LocalPositionError      PositionError    `json:"local_position_error"`
	GeographicPositionError PositionError    `json:"geographic_position_error"`
	Placement               string           `json:"placement"`
	UncertaintyModel        string           `json:"uncertainty_model"`
	PlacementSource         string           `json:"placement_source"`
	ValidFrom               time.Time        `json:"valid_from"`
	ValidUntil              time.Time        `json:"valid_until"`
	Path                    []world.FrameRef `json:"path"`
}
type ProjectedSample struct {
	Sample   sensing.Sample `json:"sample"`
	At       time.Time      `json:"at"`
	Location Result         `json:"location"`
}

// DecodeSite rejects implicit zero coordinates, missing uncertainty and truncated
// arrays. Go's normal array decoder alone would silently fill missing elements.
func DecodeSite(raw []byte) (*Site, error) {
	var cfg SiteConfig
	if err := authority.DecodeStrictLimit(raw, &cfg, 1<<20); err != nil {
		return nil, err
	}
	keys := []string{"schema_version", "site", "revision", "package_sha256", "root", "anchor", "axes", "placement", "placement_source", "valid_from", "valid_until", "frames"}
	if cfg.SchemaVersion == SiteSchemaV2 {
		keys = append(keys, "root_basis_error")
	} else if cfg.SchemaVersion != SiteSchema {
		return nil, errors.New("unsupported geographic site schema")
	}
	fields, err := required(raw, keys...)
	if err != nil {
		return nil, err
	}
	if cfg.SchemaVersion == SiteSchemaV2 {
		if err = angularErrorFields(fields["root_basis_error"]); err != nil {
			return nil, err
		}
	}
	if _, err = required(fields["root"], "id", "revision"); err != nil {
		return nil, err
	}
	if _, err = required(fields["anchor"], "latitude_degrees", "longitude_degrees", "altitude_metres", "altitude_datum", "uncertainty_metres"); err != nil {
		return nil, err
	}
	if err = exactArray(fields["axes"], 3); err != nil {
		return nil, err
	}
	var frames []json.RawMessage
	if err = json.Unmarshal(fields["frames"], &frames); err != nil {
		return nil, err
	}
	for _, frame := range frames {
		frameKeys := []string{"ref", "parent", "translation", "rotation_xyzw", "valid_from", "valid_until", "uncertainty_metres"}
		if cfg.SchemaVersion == SiteSchemaV2 {
			frameKeys = append(frameKeys, "rotation_error")
		}
		f, e := required(frame, frameKeys...)
		if e != nil {
			return nil, e
		}
		if cfg.SchemaVersion == SiteSchemaV2 {
			if e = angularErrorFields(f["rotation_error"]); e != nil {
				return nil, e
			}
		}
		for _, key := range []string{"ref", "parent"} {
			if _, e = required(f[key], "id", "revision"); e != nil {
				return nil, e
			}
		}
		if e = exactArray(f["translation"], 3); e != nil {
			return nil, e
		}
		if e = exactArray(f["rotation_xyzw"], 4); e != nil {
			return nil, e
		}
	}
	return NewSite(cfg)
}

func angularErrorFields(raw []byte) error {
	var angular world.AngularError
	if err := json.Unmarshal(raw, &angular); err != nil {
		return err
	}
	keys := []string{"known"}
	if angular.Known {
		keys = append(keys, "max_angle_radians")
	}
	if _, err := required(raw, keys...); err != nil {
		return err
	}
	return angular.Validate()
}
func required(raw []byte, keys ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if len(fields) != len(keys) {
		return nil, errors.New("unexpected geographic document fields")
	}
	for _, key := range keys {
		v, ok := fields[key]
		if !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return nil, fmt.Errorf("missing field %s", key)
		}
	}
	return fields, nil
}
func exactArray(raw []byte, n int) error {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return err
	}
	if len(values) != n {
		return errors.New("invalid coordinate array length")
	}
	for _, v := range values {
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return errors.New("null array value")
		}
	}
	return nil
}
func NewSite(cfg SiteConfig) (*Site, error) {
	if (cfg.SchemaVersion != SiteSchema && cfg.SchemaVersion != SiteSchemaV2) || cfg.Site == "" || cfg.Revision == "" || !sensing.DigestValid(cfg.PackageSHA256) || cfg.PlacementSource == "" || cfg.Axes != [3]string{"east", "up", "south"} || cfg.ValidFrom.IsZero() || !cfg.ValidUntil.After(cfg.ValidFrom) {
		return nil, errors.New("invalid geographic site metadata")
	}
	if cfg.SchemaVersion == SiteSchemaV2 {
		if cfg.RootBasisError == nil {
			return nil, errors.New("v2 requires an explicit root basis error")
		}
		if err := cfg.RootBasisError.Validate(); err != nil {
			return nil, err
		}
	} else if cfg.RootBasisError != nil {
		return nil, errors.New("v1 does not support root basis error")
	}
	for _, f := range cfg.Frames {
		if (cfg.SchemaVersion == SiteSchemaV2) != (f.RotationError != nil) {
			return nil, errors.New("frame rotation error does not match site schema")
		}
	}
	if !frameNumberValid(cfg.Anchor.UncertaintyMetres) {
		return nil, errors.New("anchor uncertainty exceeds numeric bound")
	}
	switch cfg.Placement {
	case "illustrative", "estimated", "surveyed":
	default:
		return nil, errors.New("explicit placement quality required")
	}
	graph, err := NewGraph(cfg.Root, cfg.Frames)
	if err != nil {
		return nil, err
	}
	tangent, err := NewTangent(cfg.Anchor)
	if err != nil {
		return nil, err
	}
	cfg = copySiteConfig(cfg)
	return &Site{config: cfg, graph: graph, tangent: tangent}, nil
}

func copySiteConfig(cfg SiteConfig) SiteConfig {
	cfg.RootBasisError = copyAngularError(cfg.RootBasisError)
	if cfg.Frames != nil {
		frames := make([]world.LocalFrame, len(cfg.Frames))
		copy(frames, cfg.Frames)
		for i := range frames {
			frames[i].RotationError = copyAngularError(frames[i].RotationError)
		}
		cfg.Frames = frames
	}
	return cfg
}

// Config returns an independent copy of the placement assertion. It does not
// reconstruct original signed JSON bytes; publications retain those separately.
func (s *Site) Config() SiteConfig { return copySiteConfig(s.config) }
func (s *Site) check(at time.Time, u float64) error {
	if s == nil || s.graph == nil || s.tangent == nil {
		return errors.New("geographic site is not initialized")
	}
	if at.IsZero() || at.Before(s.config.ValidFrom) || !at.Before(s.config.ValidUntil) {
		return errors.New("site placement not valid at requested time")
	}
	if !frameNumberValid(u) || u < 0 {
		return errors.New("invalid point uncertainty")
	}
	return nil
}
func (s *Site) result(from, to world.FrameRef, r TransformResult, g Coordinate, u float64, localError, geographicError PositionError) (Result, error) {
	u += s.config.Anchor.UncertaintyMetres + r.UncertaintyMetres
	if !frameNumberValid(u) {
		return Result{}, errors.New("uncertainty overflow")
	}
	start, end := s.config.ValidFrom, s.config.ValidUntil
	if r.ValidFrom.After(start) {
		start = r.ValidFrom
	}
	if !r.ValidUntil.IsZero() && r.ValidUntil.Before(end) {
		end = r.ValidUntil
	}
	return Result{Site: s.config.Site, GeoreferenceRevision: s.config.Revision, From: from, To: to, Position: r.Position, Geographic: g, UncertaintyMetres: u, UncertaintyModel: "translation-bound-only", LocalPositionError: localError, GeographicPositionError: geographicError, Placement: s.config.Placement, PlacementSource: s.config.PlacementSource, ValidFrom: start, ValidUntil: end, Path: r.Path}, nil
}
func (s *Site) Convert(req Request) (Result, error) {
	if err := s.check(req.At, req.UncertaintyMetres); err != nil {
		return Result{}, err
	}
	inputError := knownPositionError(req.UncertaintyMetres)
	r, err := s.graph.TransformWithError(req.Position, req.From, req.To, req.At, inputError)
	if err != nil {
		return Result{}, err
	}
	origin, err := s.graph.TransformWithError(req.Position, req.From, s.config.Root, req.At, inputError)
	if err != nil {
		return Result{}, err
	}
	g, err := s.tangent.ToGeographic(origin.Position)
	if err != nil {
		return Result{}, err
	}
	geographicError, err := placementError(*origin.PositionError, origin.Position, s.config.Anchor.UncertaintyMetres, s.config.RootBasisError)
	if err != nil {
		return Result{}, err
	}
	return s.result(req.From, req.To, r, g, req.UncertaintyMetres, *r.PositionError, geographicError)
}
func (s *Site) Geographic(req GeoRequest) (Result, error) {
	if err := s.check(req.At, req.UncertaintyMetres); err != nil {
		return Result{}, err
	}
	local, err := s.tangent.ToLocal(req.Coordinate)
	if err != nil {
		return Result{}, err
	}
	inputError := knownPositionError(req.UncertaintyMetres)
	rootError, err := placementError(inputError, local, s.config.Anchor.UncertaintyMetres, s.config.RootBasisError)
	if err != nil {
		return Result{}, err
	}
	r, err := s.graph.TransformWithError(local, s.config.Root, req.To, req.At, rootError)
	if err != nil {
		return Result{}, err
	}
	return s.result(s.config.Root, req.To, r, req.Coordinate, req.UncertaintyMetres, *r.PositionError, inputError)
}

// ProjectSample uses a caller-supplied capture instant because sample_ns is
// simulation-relative. It never treats that value as UTC or changes lineage.
func (s *Site) ProjectSample(sample sensing.Sample, at time.Time) (ProjectedSample, error) {
	if err := s.check(at, sample.UncertaintyM); err != nil {
		return ProjectedSample{}, err
	}
	if err := sample.Validate(); err != nil {
		return ProjectedSample{}, err
	}
	if sample.SiteID != s.config.Site || sample.PackageSHA256 != s.config.PackageSHA256 {
		return ProjectedSample{}, errors.New("sample site or package differs from georeference")
	}
	r, err := s.Convert(Request{From: sample.Frame, To: s.config.Root, Position: [3]float64(sample.PositionM), At: at, UncertaintyMetres: sample.UncertaintyM})
	if err != nil {
		return ProjectedSample{}, err
	}
	b, err := json.Marshal(sample)
	if err != nil {
		return ProjectedSample{}, err
	}
	copy, err := sensing.Decode(b)
	if err != nil {
		return ProjectedSample{}, err
	}
	return ProjectedSample{Sample: copy, At: at, Location: r}, nil
}
