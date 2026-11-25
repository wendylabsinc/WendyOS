package annotations

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

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const AmendmentType = "wendy.annotation.amendment.v1"
const AmendmentPrefix = "annotation-amendment/"
const MaxAmendments = 32
const MaxAmendmentDepth = 8
const AmendmentApp = "world-annotation-amendments-v1"

// OriginalRecord is embedded verbatim. Authority verification lives in authority
// to keep the schema independent of transport and issuer configuration.
type Amendment struct {
	OriginalRecord    json.RawMessage           `json:"original_record"`
	OriginalSHA256    string                    `json:"original_sha256"`
	PredecessorSHA256 string                    `json:"predecessor_sha256"`
	PredecessorRecord json.RawMessage           `json:"predecessor_record,omitempty"`
	Action            string                    `json:"action"`
	AuthorPrincipal   string                    `json:"author_principal"`
	Text              string                    `json:"text"`
	PositionMM        [3]int64                  `json:"position_mm"`
	AuthoredAtUnixMS  int64                     `json:"authored_at_unix_ms"`
	ExpiresAtUnixMS   int64                     `json:"expires_at_unix_ms"`
	Clock             *annotationclock.Evidence `json:"clock,omitempty"`
}

func SHA256(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func DigestValid(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func IsAmendment(c world.Component) bool {
	return c.Key.Type == AmendmentType || strings.HasPrefix(c.Key.Entity, AmendmentPrefix)
}
func Original(a Amendment) (world.Component, Note, error) {
	var r struct {
		Component world.Component `json:"component"`
	}
	if len(a.OriginalRecord) == 0 || len(a.OriginalRecord) > 24<<10 || json.Unmarshal(a.OriginalRecord, &r) != nil || (r.Component.Key.Type != Type && r.Component.Key.Type != TypeV2) {
		return r.Component, Note{}, errors.New("invalid original annotation proof")
	}
	n, e := Validate(r.Component)
	return r.Component, n, e
}
func BuildAmendment(a Amendment, cfg Config) (world.Component, error) {
	payload, e := json.Marshal(a)
	if e != nil {
		return world.Component{}, e
	}
	bound := ClockError
	if a.Clock != nil {
		interval, e := a.Clock.Interval()
		if e != nil {
			return world.Component{}, e
		}
		bound = time.Duration(interval.UpperUnixMS-interval.LowerUnixMS) * time.Millisecond
	}
	c := world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: AmendmentPrefix + SHA256(payload), Type: AmendmentType, Source: cfg.Source}, Version: world.Version{Epoch: cfg.Epoch, Sequence: cfg.Sequence}, Frame: cfg.Frame, Timing: world.Timing{SampleTime: time.UnixMilli(a.AuthoredAtUnixMS).UTC(), ClockDomain: "simulation:" + cfg.RunID, ErrorKnown: true, ErrorBound: bound}, TTL: time.Duration(a.ExpiresAtUnixMS-a.AuthoredAtUnixMS) * time.Millisecond, Provenance: world.Provenance{Origin: world.Authored, AppVersion: AmendmentApp, Simulation: &world.SimulationContext{RunID: cfg.RunID, ScenarioVersion: cfg.Frame.Revision, ClockDomain: "simulation:" + cfg.RunID}}, Payload: payload}
	_, e = ValidateAmendment(c)
	return c, e
}
func ValidateAmendment(c world.Component) (Amendment, error) { return validateAmendment(c, 0) }
func validateAmendment(c world.Component, depth int) (Amendment, error) {
	var a Amendment
	bad := func() (Amendment, error) { return a, errors.New("invalid immutable annotation amendment") }
	if depth >= MaxAmendmentDepth || len(c.Payload) > 40<<10 || json.Unmarshal(c.Payload, &a) != nil {
		return bad()
	}
	raw, e := json.Marshal(a)
	if e != nil || !bytes.Equal(raw, c.Payload) {
		return bad()
	}
	original, n, e := Original(a)
	if e != nil {
		return bad()
	}
	if a.OriginalSHA256 != SHA256(a.OriginalRecord) || !DigestValid(a.PredecessorSHA256) || a.AuthorPrincipal != n.AuthorPrincipal || a.AuthoredAtUnixMS < n.AuthoredAtUnixMS || a.AuthoredAtUnixMS >= n.ExpiresAtUnixMS || a.ExpiresAtUnixMS != n.ExpiresAtUnixMS {
		return bad()
	}
	if a.PredecessorSHA256 == a.OriginalSHA256 {
		if len(a.PredecessorRecord) != 0 {
			return bad()
		}
	} else {
		var prior struct {
			Component world.Component `json:"component"`
		}
		if len(a.PredecessorRecord) == 0 || json.Unmarshal(a.PredecessorRecord, &prior) != nil {
			return bad()
		}
		before, e := validateAmendment(prior.Component, depth+1)
		if e != nil || SHA256(prior.Component.Payload) != a.PredecessorSHA256 || !bytes.Equal(before.OriginalRecord, a.OriginalRecord) || before.Action == "withdraw" || a.AuthoredAtUnixMS < before.AuthoredAtUnixMS {
			return bad()
		}
	}
	if a.Action != "correct" && a.Action != "withdraw" {
		return bad()
	}
	if a.Action == "withdraw" {
		if a.Text != "" || a.PositionMM != n.PositionMM {
			return bad()
		}
	} else if strings.TrimSpace(a.Text) == "" || len(a.Text) > MaxTextBytes || !utf8.ValidString(a.Text) {
		return bad()
	}
	for _, v := range a.PositionMM {
		if v < -MaxPositionMM || v > MaxPositionMM {
			return bad()
		}
	}
	bound := ClockError
	if a.Clock != nil {
		interval, e := a.Clock.Interval()
		if e != nil || interval.LowerUnixMS != a.AuthoredAtUnixMS {
			return bad()
		}
		bound = time.Duration(interval.UpperUnixMS-interval.LowerUnixMS) * time.Millisecond
		o := a.Clock.Observation.Observation
		if o.Tenant != c.Key.Tenant || o.Site != c.Key.Site || o.OperatorPrincipal != a.AuthorPrincipal || o.ClockDomain != c.Timing.ClockDomain {
			return bad()
		}
	}
	if c.Key.Type != AmendmentType || c.Key.Entity != AmendmentPrefix+SHA256(c.Payload) || c.Key.Tenant != original.Key.Tenant || c.Key.Site != original.Key.Site || c.Key.Source != original.Key.Source || c.Frame != original.Frame || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Tombstone {
		return bad()
	}
	if c.Timing.Validate() != nil || !c.Timing.ErrorKnown || c.Timing.ErrorBound != bound || !c.Timing.SampleTime.Equal(time.UnixMilli(a.AuthoredAtUnixMS)) || c.TTL != time.Duration(a.ExpiresAtUnixMS-a.AuthoredAtUnixMS)*time.Millisecond {
		return bad()
	}
	p := c.Provenance
	s := p.Simulation
	orig := original.Provenance.Simulation
	if p.Validate() != nil || p.Origin != world.Authored || p.AppVersion != AmendmentApp || s == nil || orig == nil || *s != *orig || c.Timing.ClockDomain != original.Timing.ClockDomain {
		return bad()
	}
	return a, nil
}

// View is a projection of an authenticated local replica, never global
// completeness or independent origin verification. A withdrawal is terminal.
type View struct {
	OriginalSHA256  string          `json:"original_sha256"`
	Original        world.Component `json:"original"`
	State           string          `json:"state"`
	Text            string          `json:"text"`
	PositionMM      [3]int64        `json:"position_mm"`
	HeadSHA256      string          `json:"head_sha256"`
	ExpiresAtUnixMS int64           `json:"expires_at_unix_ms"`
	AmendmentSHA256 []string        `json:"amendment_sha256"`
	Conflict        bool            `json:"conflict"`
}

func Resolve(originalRaw json.RawMessage, components []world.Component, now time.Time) (View, error) {
	original, n, e := Original(Amendment{OriginalRecord: originalRaw})
	if e != nil {
		return View{}, e
	}
	root := SHA256(originalRaw)
	v := View{OriginalSHA256: root, Original: original, State: "original", Text: n.Text, PositionMM: n.PositionMM, HeadSHA256: root, ExpiresAtUnixMS: n.ExpiresAtUnixMS, AmendmentSHA256: []string{}}
	byID := map[string]Amendment{}
	children := map[string][]string{}
	withdraw := false
	queue := append([]world.Component(nil), components...)
	for i := 0; i < len(queue); i++ {
		c := queue[i]
		if !IsAmendment(c) {
			continue
		}
		a, e := ValidateAmendment(c)
		if e != nil {
			return View{}, e
		}
		if a.OriginalSHA256 != root {
			continue
		}
		id := SHA256(c.Payload)
		if _, ok := byID[id]; ok {
			continue
		}
		byID[id] = a
		if len(a.PredecessorRecord) > 0 {
			var prior struct {
				Component world.Component `json:"component"`
			}
			json.Unmarshal(a.PredecessorRecord, &prior)
			queue = append(queue, prior.Component)
		}
		children[a.PredecessorSHA256] = append(children[a.PredecessorSHA256], id)
		v.AmendmentSHA256 = append(v.AmendmentSHA256, id)
		if a.Action == "withdraw" {
			withdraw = true
		}
	}
	sort.Strings(v.AmendmentSHA256)
	for _, ids := range children {
		if len(ids) > 1 {
			v.Conflict = true
		}
	}
	switch {
	case now.UnixMilli() >= n.ExpiresAtUnixMS:
		v.State = "expired"
		v.Text = ""
		return v, nil
	case len(byID) > MaxAmendments:
		v.State = "incomplete"
		v.Text = ""
		return v, nil
	case withdraw:
		v.State = "withdrawn"
		v.Text = ""
		return v, nil
	case v.Conflict:
		v.State = "conflict"
		v.Text = ""
		return v, nil
	}
	visited := map[string]bool{}
	for len(children[v.HeadSHA256]) == 1 {
		id := children[v.HeadSHA256][0]
		if visited[id] {
			v.State = "incomplete"
			v.Text = ""
			return v, nil
		}
		visited[id] = true
		a := byID[id]
		v.HeadSHA256 = id
		v.Text = a.Text
		v.PositionMM = a.PositionMM
		v.State = "corrected"
	}
	if len(visited) != len(byID) {
		v.State = "incomplete"
		v.Text = ""
	}
	return v, nil
}
