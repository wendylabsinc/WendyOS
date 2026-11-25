// Package annotations defines immutable, human-authored simulation notes.
// A note grants no sensor, zone, task, or site publication authority.
package annotations

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	Type         = "wendy.annotation.note.v1"
	TypeV2       = "wendy.annotation.note.v2"
	AppVersionV2 = "world-annotations-v2"
	EntityPrefix = "annotation/"
	AppVersion   = "world-annotations-v1"
	MaxTextBytes = 2048
	MaxTTL       = 24 * time.Hour
	// ClockError is an explicit shared-host simulation wall-clock assumption.
	// It is not a claim of calibrated physical capture time.
	ClockError          = time.Second
	MaxPositionMM int64 = 1_000_000_000
)

var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// Note's field order and integer coordinates form the canonical wire payload.
// PositionMM uses the component's right-handed, Y-up frame, in millimetres.
type Note struct {
	ID               string                    `json:"id"`
	AuthorPrincipal  string                    `json:"author_principal"`
	Text             string                    `json:"text"`
	PositionMM       [3]int64                  `json:"position_mm"`
	AuthoredAtUnixMS int64                     `json:"authored_at_unix_ms"`
	ExpiresAtUnixMS  int64                     `json:"expires_at_unix_ms"`
	Clock            *annotationclock.Evidence `json:"clock,omitempty"`
}

type Config struct {
	Tenant, Site, RunID string
	Source              world.Source
	Frame               world.FrameRef
	Epoch, Sequence     uint64
}

func Reserved(c world.Component) bool {
	return IsAmendment(c) || (c.Key.Type == Type || c.Key.Type == TypeV2) || strings.HasPrefix(c.Key.Entity, EntityPrefix)
}

func Build(note Note, cfg Config) (world.Component, error) {
	payload, err := json.Marshal(note)
	if err != nil {
		return world.Component{}, err
	}
	c := world.Component{
		Key:     world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: EntityPrefix + note.ID, Type: Type, Source: cfg.Source},
		Version: world.Version{Epoch: cfg.Epoch, Sequence: cfg.Sequence}, Frame: cfg.Frame,
		Timing:     world.Timing{SampleTime: time.UnixMilli(note.AuthoredAtUnixMS).UTC(), ClockDomain: "simulation:" + cfg.RunID, ErrorKnown: true, ErrorBound: ClockError},
		TTL:        time.Duration(note.ExpiresAtUnixMS-note.AuthoredAtUnixMS) * time.Millisecond,
		Provenance: world.Provenance{Origin: world.Authored, AppVersion: AppVersion, Simulation: &world.SimulationContext{RunID: cfg.RunID, ScenarioVersion: cfg.Frame.Revision, ClockDomain: "simulation:" + cfg.RunID}},
		Payload:    payload,
	}
	if note.Clock != nil {
		interval, e := note.Clock.Interval()
		if e != nil {
			return world.Component{}, e
		}
		c.Key.Type = TypeV2
		c.Provenance.AppVersion = AppVersionV2
		c.Timing.ErrorBound = time.Duration(interval.UpperUnixMS-interval.LowerUnixMS) * time.Millisecond
	}
	_, err = Validate(c)
	return c, err
}

// Validate is independent of wall-clock freshness, so retained expired records
// can be checked on reopen. Canonical equality rejects unknown/duplicate fields,
// omitted fields, extra array coordinates and alternate JSON representations.
func Validate(c world.Component) (Note, error) {
	if IsAmendment(c) {
		a, e := ValidateAmendment(c)
		return Note{AuthorPrincipal: a.AuthorPrincipal, Text: a.Text, PositionMM: a.PositionMM, AuthoredAtUnixMS: a.AuthoredAtUnixMS, ExpiresAtUnixMS: a.ExpiresAtUnixMS, Clock: a.Clock}, e
	}

	var n Note
	if len(c.Payload) > 16<<10 || json.Unmarshal(c.Payload, &n) != nil {
		return n, errors.New("invalid annotation payload")
	}
	canonical, err := json.Marshal(n)
	if err != nil || !bytes.Equal(canonical, c.Payload) {
		return n, errors.New("annotation payload must use exact canonical schema")
	}
	if !uuid.MatchString(n.ID) || n.AuthorPrincipal == "" || len(n.AuthorPrincipal) > 256 || !utf8.ValidString(n.AuthorPrincipal) || !utf8.ValidString(n.Text) || len(n.Text) > MaxTextBytes || strings.TrimSpace(n.Text) == "" {
		return n, errors.New("invalid annotation identity, author or text")
	}
	for _, coordinate := range n.PositionMM {
		if coordinate < -MaxPositionMM || coordinate > MaxPositionMM {
			return n, errors.New("annotation position exceeds bound")
		}
	}
	if n.AuthoredAtUnixMS <= 0 || n.ExpiresAtUnixMS <= n.AuthoredAtUnixMS || n.ExpiresAtUnixMS-n.AuthoredAtUnixMS > MaxTTL.Milliseconds() {
		return n, errors.New("invalid annotation lifetime")
	}
	wantType, wantApp, wantError := Type, AppVersion, ClockError
	if n.Clock != nil {
		interval, err := n.Clock.Interval()
		if err != nil || interval.LowerUnixMS != n.AuthoredAtUnixMS {
			return n, errors.New("annotation clock interval differs from draft")
		}
		wantType, wantApp, wantError = TypeV2, AppVersionV2, time.Duration(interval.UpperUnixMS-interval.LowerUnixMS)*time.Millisecond
		o := n.Clock.Observation.Observation
		q := n.Clock.Request.Request
		if q.Validate() != nil || o.Tenant != c.Key.Tenant || o.Site != c.Key.Site || o.OperatorPrincipal != n.AuthorPrincipal || o.ClockDomain != c.Timing.ClockDomain || q.Tenant != o.Tenant || q.Site != o.Site || q.Audience != o.Audience || q.RunID != o.RunID || q.ClockDomain != o.ClockDomain || q.Nonce != o.Nonce {
			return n, errors.New("annotation clock scope mismatch")
		}
	}
	if c.Key.Type != wantType || c.Key.Entity != EntityPrefix+n.ID || c.Key.Tenant == "" || c.Key.Site == "" || c.Key.Source.Validate() != nil || c.Frame.Validate() != nil || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Tombstone {
		return n, errors.New("invalid annotation envelope; edits and tombstones are unsupported")
	}
	if c.Timing.Validate() != nil || !c.Timing.ErrorKnown || c.Timing.ErrorBound != wantError || c.Timing.SampleTime.Format(time.RFC3339Nano) != time.UnixMilli(n.AuthoredAtUnixMS).UTC().Format(time.RFC3339Nano) || c.TTL != time.Duration(n.ExpiresAtUnixMS-n.AuthoredAtUnixMS)*time.Millisecond {
		return n, errors.New("annotation timing differs from immutable draft")
	}
	p := c.Provenance
	s := p.Simulation
	if p.Validate() != nil || p.Origin != world.Authored || p.AppVersion != wantApp || s == nil || len(s.RunID) > 256 || s.ClockDomain != "simulation:"+s.RunID || c.Timing.ClockDomain != s.ClockDomain || s.ScenarioVersion != c.Frame.Revision || s.Seed != 0 {
		return n, errors.New("annotation requires authored simulation provenance and frame binding")
	}
	return n, nil
}
