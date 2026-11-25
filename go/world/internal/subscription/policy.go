// Package subscription describes an immutable local storage interest, not an
// authority grant. Every admitted origin still requires normal verification.
package subscription

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Version = 1
const MaxBytes = 64 << 10

var ErrMismatch = errors.New("replica subscription does not match its durable policy; create a new replica to change scope")
var ErrExcluded = errors.New("component is outside the replica subscription")

type Policy struct {
	UnknownLocation string              `json:"unknown_location,omitempty"`
	Version         int                 `json:"version"`
	Tenant          string              `json:"tenant"`
	Site            string              `json:"site"`
	Selection       selection.Selection `json:"selection"`
	RequiredTypes   []string            `json:"required_types"`
}

func (p Policy) Normalize() (Policy, error) {
	bounded := func(s string) bool {
		return s != "" && len(s) <= 256 && utf8.ValidString(s) && strings.TrimSpace(s) == s
	}
	if p.Version != Version || !bounded(p.Tenant) || !bounded(p.Site) {
		return Policy{}, errors.New("subscription requires version 1 and bounded tenant/site")
	}
	s, err := p.Selection.Normalize()
	if err != nil {
		return Policy{}, err
	}
	if s.Region != nil {
		if p.UnknownLocation != "retain" {
			return Policy{}, errors.New("regional storage requires explicit unknown_location retain")
		}
		for _, typ := range s.Types {
			if typ != "wendy.sensor.v1" && typ != "wendy.hazard.v1" {
				return Policy{}, errors.New("regional storage supports sensor and hazard records only")
			}
		}
	} else if p.UnknownLocation != "" {
		return Policy{}, errors.New("unknown_location is only valid for regional storage")
	}
	if len(s.Frames) != 0 {
		return Policy{}, errors.New("replica subscriptions select immutable keys, not frame revisions")
	}
	if len(s.Types)+len(s.Entities)+len(s.Sources) == 0 {
		return Policy{}, errors.New("partial replica requires a nonempty key selection; omit policy for a full replica")
	}
	if len(p.RequiredTypes) != 1 || p.RequiredTypes[0] != sitepub.Type {
		return Policy{}, errors.New("subscription must explicitly retain wendy.site.georeference.v1 metadata")
	}
	p.Selection = s
	p.RequiredTypes = []string{sitepub.Type}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > 48<<10 {
		return Policy{}, errors.New("subscription exceeds byte bound")
	}
	return p, nil
}

// Allows is called only on a normalized policy. A tombstone has the same key as
// the value it removes, so dropping payload/frame selectors cannot drop deletes.
func (p *Policy) Allows(k world.Key) bool {
	if p == nil {
		return true
	}
	if k.Tenant != p.Tenant || k.Site != p.Site {
		return false
	}
	if k.Type == sitepub.Type {
		return true
	}
	s := p.Selection
	s.Region = nil
	return s.Matches(world.Component{Key: k})
}
func (p *Policy) Digest() string {
	if p == nil {
		return ""
	}
	return selection.Digest(p)
}
func (p *Policy) Clone() *Policy {
	if p == nil {
		return nil
	}
	copy, err := p.Normalize()
	if err != nil {
		panic("invalid internal subscription")
	}
	return &copy
}
func (p *Policy) Scope() *selection.ReplicaScope {
	if p == nil {
		return nil
	}
	copy := p.Clone()
	return &selection.ReplicaScope{UnknownLocation: p.UnknownLocation, Partial: true, Tenant: p.Tenant, Site: p.Site, PolicySHA256: p.Digest(), Selection: copy.Selection, RequiredTypes: copy.RequiredTypes}
}
func Equal(a, b *Policy) bool { return a.Digest() == b.Digest() }

// Retains separates immutable key interest from payload location. Unknown
// locations are retained; only an authenticated, definitely outside value fences.
func (p *Policy) Retains(c world.Component) bool {
	if !p.Allows(c.Key) {
		return false
	}
	return p == nil || p.Selection.Region == nil || c.Key.Type == sitepub.Type || c.Tombstone || p.Selection.Region.Classify(c).Class != "outside"
}
func (p *Policy) Regional() bool { return p != nil && p.Selection.Region != nil }
