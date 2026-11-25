package agentmodule

import (
	"context"
	"encoding/hex"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

func normalizedScopes(in []serviceorigin.PublishScope) []serviceorigin.PublishScope {
	out := append([]serviceorigin.PublishScope(nil), in...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].ComponentType != out[j].ComponentType {
			return out[i].ComponentType < out[j].ComponentType
		}
		return out[i].Stream < out[j].Stream
	})
	return out
}
func equalScopes(a, b serviceorigin.Scope) bool {
	a.Publish = normalizedScopes(a.Publish)
	b.Publish = normalizedScopes(b.Publish)
	return reflect.DeepEqual(a, b)
}
func (p Policy) scope(g Grant, generation uint64) serviceorigin.Scope {
	return serviceorigin.Scope{Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: g.App, Service: g.Service, DeploymentGeneration: generation, ExecutionDomain: p.ExecutionDomain, Publish: normalizedScopes(g.Publish)}
}
func (p Policy) validate() error {
	return p.validateWithReplication(nil)
}
func (p Policy) validateWithReplication(replica *ReplicaConfig) error {
	if p.Version != 1 || p.NotBefore.IsZero() || !p.NotAfter.After(p.NotBefore) || p.NotAfter.Sub(p.NotBefore) > 24*time.Hour || p.SampleClockDomain == "" || len(p.SampleClockDomain) > 256 || len(p.Grants) == 0 || len(p.Grants) > 64 || len(p.Frames) == 0 || len(p.Frames) > 64 || len(p.DeniedCertificates) > 256 || len(p.DeniedKeys) > 256 {
		return ErrDenied
	}
	if p.ExecutionDomain == "physical" && p.SampleClockDomain != "utc" || p.ExecutionDomain == "simulation" && (!strings.HasPrefix(p.SampleClockDomain, "simulation:") || len(p.SampleClockDomain) == len("simulation:")) {
		return ErrDenied
	}
	seen := map[string]bool{}
	for _, f := range p.Frames {
		if f.Validate() != nil || seen[f.ID] {
			return ErrDenied
		}
		seen[f.ID] = true
	}
	seen = map[string]bool{}
	for _, g := range p.Grants {
		key := g.App + "\x00" + g.Service
		if seen[key] || p.scope(g, 1).Validate() != nil || len(g.Publish)+len(g.Subscribe) == 0 || serviceaccess.ValidateSources(g.Subscribe, serviceaccess.MaxSubscriptions) != nil {
			return ErrDenied
		}
		seen[key] = true
		for _, s := range g.Publish {
			if !knownSchema(s.ComponentType) {
				return ErrDenied
			}
		}
		for _, s := range g.Subscribe {
			resolved, err := s.Resolve(p.Device)
			if err != nil || !knownSchema(s.ComponentType) || resolved.Device != p.Device && !replica.imports(p, resolved) {
				return ErrDenied
			}
		}
	}
	for _, list := range [][]string{p.DeniedCertificates, p.DeniedKeys} {
		seen = map[string]bool{}
		for _, s := range list {
			b, e := hex.DecodeString(s)
			if e != nil || len(b) != 32 || hex.EncodeToString(b) != s || seen[s] {
				return ErrDenied
			}
			seen[s] = true
		}
	}
	return nil
}

func normalizedSubscriptions(in []serviceaccess.Source) []serviceaccess.Source {
	out := append([]serviceaccess.Source(nil), in...)
	sort.Slice(out, func(i, j int) bool { return serviceaccess.Key(out[i], "") < serviceaccess.Key(out[j], "") })
	return out
}
func equalSubscriptions(a, b []serviceaccess.Source) bool {
	return reflect.DeepEqual(normalizedSubscriptions(a), normalizedSubscriptions(b))
}
func (m *Manager) permitsAccess(scope serviceorigin.Scope, subscribe []serviceaccess.Source) bool {
	if len(scope.Publish)+len(subscribe) == 0 || !m.permits(scope) || serviceaccess.ValidateSources(subscribe, serviceaccess.MaxSubscriptions) != nil {
		return false
	}
	for _, g := range m.cfg.Policy.Grants {
		if g.App != scope.App || g.Service != scope.Service {
			continue
		}
		for _, requested := range subscribe {
			found := false
			for _, allowed := range g.Subscribe {
				if requested == allowed {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return false
}
func (m *Manager) policyCurrent(ctx context.Context) (err error) {
	defer func() {
		if err != nil && ctx != nil && ctx.Err() == nil {
			m.failReplication("policy", err)
		}
	}()
	if ctx == nil || ctx.Err() != nil {
		return ErrDenied
	}
	if m.policyFailed.Load() {
		return ErrDenied
	}
	if err := m.configCurrent(ctx); err != nil {
		if ctx.Err() == nil {
			m.policyFailed.Store(true)
		}
		return ErrDenied
	}
	w, err := m.physicalNow()
	if err != nil || w.Validate() != nil || w.Earliest.Before(m.cfg.Policy.NotBefore) || !w.Latest.Before(m.cfg.Policy.NotAfter) {
		m.policyFailed.Store(true)
		return ErrDenied
	}
	if m.policyFailed.Load() {
		return ErrDenied
	}
	return nil
}
func (m *Manager) permits(scope serviceorigin.Scope) bool {
	if scope.Validate() != nil || scope.Tenant != m.cfg.Policy.Tenant || scope.Site != m.cfg.Policy.Site || scope.Device != m.cfg.Policy.Device || scope.ExecutionDomain != m.cfg.Policy.ExecutionDomain {
		return false
	}
	for _, g := range m.cfg.Policy.Grants {
		if g.App != scope.App || g.Service != scope.Service {
			continue
		}
		for _, requested := range scope.Publish {
			found := false
			for _, allowed := range g.Publish {
				if requested == allowed {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	return false
}
func (m *Manager) authorize(ctx context.Context, claims serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
	if m.policyCurrent(ctx) != nil || !m.permits(claims.Scope) || id.Principal != m.cfg.Policy.Device {
		return ErrDenied
	}
	if m.identityAllowed(id) != nil {
		return ErrDenied
	}
	m.mu.RLock()
	b := m.bindings[bindingKey(claims.Scope.App, claims.Scope.Service)]
	allowed := b != nil && equalScopes(b.scope, claims.Scope) && b.epoch == claims.Epoch && m.deps.Identity.Current(b.token)
	m.mu.RUnlock()
	if !allowed {
		return ErrDenied
	}
	return nil
}
func (m *Manager) identityAllowed(id wendycert.DeviceIdentity) error {
	if id.Principal != m.cfg.Policy.Device || id.Tenant != m.cfg.Policy.Tenant {
		return ErrDenied
	}
	for _, denied := range m.cfg.Policy.DeniedCertificates {
		for _, candidate := range id.CandidateCertificateSHA256() {
			if denied == candidate {
				return ErrDenied
			}
		}
	}
	for _, denied := range m.cfg.Policy.DeniedKeys {
		for _, candidate := range id.CandidateSigningKeySHA256() {
			if denied == candidate {
				return ErrDenied
			}
		}
	}
	return nil
}
