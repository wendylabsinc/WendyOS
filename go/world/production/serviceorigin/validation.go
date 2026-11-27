package serviceorigin

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// Match the pinned agent's app/service naming limits. These names remain
// separate fields rather than a delimiter-joined caller-supplied identity.
var appName = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,253}$`)
var serviceName = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,55}[a-z0-9])?$`)

func bounded(s string, n int) bool {
	if s == "" || len(s) > n || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
func canonicalUUID(s string) bool { id, err := uuid.Parse(s); return err == nil && id.String() == s }

func validateScope(s Scope) error {
	const devicePrefix = "spiffe://wendy.sh/tenant/"
	prefix := devicePrefix + s.Tenant + "/device/"
	if !canonicalUUID(s.Tenant) || !bounded(s.Site, 256) || !strings.HasPrefix(s.Device, prefix) || !canonicalUUID(strings.TrimPrefix(s.Device, prefix)) || !appName.MatchString(s.App) || strings.Trim(s.App, ".") == "" || !serviceName.MatchString(s.Service) || s.DeploymentGeneration == 0 || s.ExecutionDomain != "physical" && s.ExecutionDomain != "simulation" || len(s.Publish) > MaxPublishScopes {
		return ErrDenied
	}
	seen := map[PublishScope]bool{}
	for _, p := range s.Publish {
		if !bounded(p.ComponentType, 256) || !bounded(p.Stream, 256) || strings.ContainsAny(p.ComponentType+p.Stream, "*?") || seen[p] {
			return ErrDenied
		}
		seen[p] = true
	}
	return nil
}

func validateRequest(p PublishRequest, s Scope) error {
	if !bounded(p.Entity, 256) || !bounded(p.ComponentType, 256) || !bounded(p.Stream, 256) || p.Frame.Validate() != nil || !bounded(p.Frame.ID, 256) || !bounded(p.Frame.Revision, 256) || p.Timing.Validate() != nil || !bounded(p.Timing.ClockDomain, 256) || p.TTL <= 0 || p.TTL > 30*24*time.Hour || p.Timing.SampleTime.Year() > 9999 || p.Timing.SampleTime.Year() < 1 || p.Timing.ErrorBound > 24*time.Hour || p.Timing.AgeHint > 30*24*time.Hour || p.Provenance.Validate() != nil || !bounded(p.Provenance.AppVersion, 256) || len(p.Provenance.Inputs) > 256 || len(p.Payload) > MaxPayloadBytes {
		return ErrDenied
	}
	allowed := false
	for _, scope := range s.Publish {
		if scope.ComponentType == p.ComponentType && scope.Stream == p.Stream {
			allowed = true
			break
		}
	}
	if !allowed {
		return ErrDenied
	}
	if p.Tombstone {
		if len(p.Payload) != 0 {
			return ErrDenied
		}
	} else {
		// Treat payload as opaque schema bytes but reject JSON ambiguity before
		// certifying it. Nested unknown schema fields are intentionally allowed.
		var payload map[string]any
		if jws.DecodeStrict(p.Payload, &payload) != nil {
			return ErrDenied
		}
	}
	if s.ExecutionDomain == "physical" {
		if p.Provenance.Synthetic() || strings.HasPrefix(p.Timing.ClockDomain, "simulation:") || strings.HasPrefix(p.Timing.ClockDomain, "replay:") {
			return ErrDenied
		}
	} else {
		// This first format admits original simulation or wholly simulated
		// flattened ancestry. Replay and mixed datasets need a separate adapter.
		var simulation *world.SimulationContext
		switch p.Provenance.Origin {
		case world.Simulated, world.Authored:
			simulation = p.Provenance.Simulation
		case world.Inferred:
			for _, input := range p.Provenance.Inputs {
				if input.Origin != world.Simulated || input.Simulation == nil {
					return ErrDenied
				}
				if simulation != nil && *simulation != *input.Simulation {
					return ErrDenied
				}
				simulation = input.Simulation
			}
		default:
			return ErrDenied
		}
		if simulation == nil || !bounded(simulation.RunID, 256) || !bounded(simulation.ScenarioVersion, 256) || simulation.ClockDomain != "simulation:"+simulation.RunID || p.Timing.ClockDomain != simulation.ClockDomain {
			return ErrDenied
		}
	}
	return nil
}

func validateClaims(c DelegationClaims) error {
	if c.Version != Version || c.AuthorityDomain != AuthorityDomain || validateScope(c.Scope) != nil || c.Epoch == 0 || len(c.PublicKey) != ed25519.PublicKeySize || !validWindow(c.IssuedAtUnixMS, c.ExpiresAtUnixMS) {
		return ErrDenied
	}
	return nil
}

func validWindow(from, until int64) bool {
	return from > 0 && until > from && until-from <= MaxLifetime.Milliseconds() && time.UnixMilli(until).Year() <= 9999
}
func within(w trustedtime.Interval, from, until int64) bool {
	return w.Validate() == nil && !w.Earliest.Before(time.UnixMilli(from)) && w.Latest.Before(time.UnixMilli(until))
}
func forward(a, b trustedtime.Interval) bool {
	return b.Validate() == nil && a.BootID == b.BootID && b.Monotonic >= a.Monotonic && !b.Earliest.Before(a.Earliest) && !b.Latest.Before(a.Latest)
}
func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func cloneScope(s Scope) Scope { s.Publish = append([]PublishScope(nil), s.Publish...); return s }
func cloneClaims(c DelegationClaims) DelegationClaims {
	c.Scope = cloneScope(c.Scope)
	c.PublicKey = bytes.Clone(c.PublicKey)
	return c
}
func cloneDelegation(d SignedDelegation) SignedDelegation {
	d.Claims = bytes.Clone(d.Claims)
	d.Signature = bytes.Clone(d.Signature)
	chain := make([][]byte, len(d.ChainDER))
	for i, b := range d.ChainDER {
		chain[i] = bytes.Clone(b)
	}
	d.ChainDER = chain
	return d
}
func cloneRequest(p PublishRequest) PublishRequest {
	p.Payload = bytes.Clone(p.Payload)
	cloneSim := func(s *world.SimulationContext) *world.SimulationContext {
		if s == nil {
			return nil
		}
		copy := *s
		return &copy
	}
	cloneTime := func(t *time.Time) *time.Time {
		if t == nil {
			return nil
		}
		copy := *t
		return &copy
	}
	p.Provenance.Simulation = cloneSim(p.Provenance.Simulation)
	p.Provenance.OriginalCaptureTime = cloneTime(p.Provenance.OriginalCaptureTime)
	p.Provenance.Inputs = append([]world.Input(nil), p.Provenance.Inputs...)
	for i := range p.Provenance.Inputs {
		p.Provenance.Inputs[i].Simulation = cloneSim(p.Provenance.Inputs[i].Simulation)
		p.Provenance.Inputs[i].OriginalCaptureTime = cloneTime(p.Provenance.Inputs[i].OriginalCaptureTime)
	}
	return p
}

func decodeRecord(raw []byte, c DelegationClaims, delegation []byte) (Record, error) {
	var r Record
	if len(raw) == 0 || len(raw) > MaxRecordBytes || jws.DecodeStrict(raw, &r) != nil || r.Version != Version || r.AuthorityDomain != AuthorityDomain || r.DelegationSHA256 != hash(delegation) || !reflect.DeepEqual(r.Scope, c.Scope) || r.Epoch != c.Epoch || r.Sequence == 0 || !validWindow(r.IssuedAtUnixMS, r.ExpiresAtUnixMS) || r.IssuedAtUnixMS < c.IssuedAtUnixMS || r.ExpiresAtUnixMS > c.ExpiresAtUnixMS || validateRequest(r.Publication, r.Scope) != nil {
		return Record{}, ErrDenied
	}
	return r, nil
}

func marshalBounded(v any, limit int) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil || len(b) > limit {
		return nil, ErrDenied
	}
	return b, nil
}
