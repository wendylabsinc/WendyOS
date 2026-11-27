package agentmodule

import (
	"bytes"
	"context"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

// Authority metadata is local actor state. It makes no assertion that any
// observation exists, remains live, passes schema/media checks or has not been
// tombstoned. Initial Query and original-proof validation retain those duties.
func (m *Manager) readSourceAuthority(ctx context.Context, scope serviceorigin.Scope, nonce []byte, sources []serviceaccess.Source, fence *appservice.Fence) (*appv1.SourceAuthorityReply, error) {
	if len(nonce) != appv1.ClockNonceSize || len(sources) == 0 || len(sources) > appv1.MaxAuthoritySources {
		return nil, ErrDenied
	}
	sources = append([]serviceaccess.Source(nil), sources...)
	resolved := make(map[serviceaccess.Source]bool, len(sources))
	for _, source := range sources {
		local, e := source.Resolve(scope.Device)
		if e != nil || local.Device != scope.Device || resolved[local] {
			return nil, ErrDenied
		}
		resolved[local] = true
	}
	allowed := func(reader *binding) bool {
		for _, source := range sources {
			if !subscribed(reader, source) {
				return false
			}
		}
		return true
	}
	var reply *appv1.SourceAuthorityReply
	err := m.accessFenced(ctx, scope, fence, allowed, func(ctx context.Context) error {
		// Acquire a fresh physical interval after policy work. There is no
		// sample clock, journal, evidence, lifecycle or publisher signing call.
		w, e := m.physicalNow()
		if e != nil || ctx.Err() != nil || !physicalWireTime(w.Earliest) || !physicalWireTime(w.Latest) {
			return ErrDenied
		}
		m.mu.RLock()
		defer m.mu.RUnlock()
		if !m.readerCurrentLocked(scope, w) {
			return ErrDenied
		}
		reader := m.bindings[bindingKey(scope.App, scope.Service)]
		if !allowed(reader) || m.identityAllowed(reader.device) != nil {
			return ErrDenied
		}
		reply = &appv1.SourceAuthorityReply{Nonce: bytes.Clone(nonce), Physical: &appv1.PhysicalClock{EarliestUnixNs: w.Earliest.UnixNano(), LatestUnixNs: w.Latest.UnixNano(), BootId: w.BootID, MonotonicNs: int64(w.Monotonic), Source: w.Source}, Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, ExecutionDomain: scope.ExecutionDomain, DeploymentGeneration: scope.DeploymentGeneration, ServerBootId: m.clockIncarnation}
		for _, source := range sources {
			b := m.bindings[bindingKey(source.App, source.Service)]
			if !m.sourceAuthorityCurrentLocked(scope, source, b, w) {
				return ErrDenied
			}
			reply.Authorities = append(reply.Authorities, &appv1.SourceAuthority{Source: &appv1.SourceSelector{App: source.App, Service: source.Service, ComponentType: source.ComponentType, Stream: source.Stream, Device: source.Device}, SignedDelegationJson: bytes.Clone(b.delegation)})
		}
		_, e = appv1.DecodeSourceAuthorityReply(reply)
		return e
	}, m.withCurrentClock)
	if err != nil {
		return nil, err
	}
	return reply, nil
}

// Caller holds m.mu.RLock. All fields are construction-time owned metadata;
// this does not call Publisher.Validate or reacquire any service mutex.
func (m *Manager) sourceAuthorityCurrentLocked(reader serviceorigin.Scope, source serviceaccess.Source, b *binding, w trustedtime.Interval) bool {
	local, e := source.Resolve(reader.Device)
	if e != nil || local.Device != reader.Device || b == nil || b.scope.Tenant != reader.Tenant || b.scope.Site != reader.Site || b.scope.Device != reader.Device || b.scope.ExecutionDomain != reader.ExecutionDomain || b.scope.App != source.App || b.scope.Service != source.Service || b.epoch == 0 || b.publisherDone == nil || len(b.delegation) == 0 || !m.deps.Identity.Current(b.token) || m.identityAllowed(b.device) != nil {
		return false
	}
	select {
	case <-b.publisherDone:
		return false
	default:
	}
	c := b.delegationClaims
	return !m.closed && !m.policyFailed.Load() && m.permits(b.scope) && publishes(b.scope, serviceorigin.PublishScope{ComponentType: source.ComponentType, Stream: source.Stream}) && equalScopes(c.Scope, b.scope) && c.Epoch == b.epoch && c.Version == serviceorigin.Version && c.AuthorityDomain == serviceorigin.AuthorityDomain && c.IssuedAtUnixMS == b.authorityFrom.UnixMilli() && c.ExpiresAtUnixMS == b.authorityUntil.UnixMilli() && !w.Earliest.Before(b.authorityFrom) && w.Latest.Before(b.authorityUntil) && w.Latest.Before(b.device.ValidUntil)
}

func (m *Manager) sourceAuthorityResponseCurrent(scope serviceorigin.Scope, reply *appv1.SourceAuthorityReply) bool {
	original, e := appv1.DecodeSourceAuthorityReply(reply)
	if e != nil || reply.ServerBootId != m.clockIncarnation || reply.Tenant != scope.Tenant || reply.Site != scope.Site || reply.Device != scope.Device || reply.App != scope.App || reply.Service != scope.Service || reply.ExecutionDomain != scope.ExecutionDomain || reply.DeploymentGeneration != scope.DeploymentGeneration {
		return false
	}
	// This is the last possibly blocking observation. Every source and the
	// reader use this one interval; original reply bytes are never refreshed.
	w, e := m.physicalNow()
	if e != nil || w.BootID != original.BootID || w.Monotonic < original.Monotonic || w.Monotonic-original.Monotonic > appv1.MaxClockResponseAge || w.Earliest.Before(original.Earliest) || w.Latest.Before(original.Latest) {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.readerCurrentLocked(scope, w) {
		return false
	}
	reader := m.bindings[bindingKey(scope.App, scope.Service)]
	if m.identityAllowed(reader.device) != nil {
		return false
	}
	seen := map[serviceaccess.Source]bool{}
	for _, a := range reply.Authorities {
		q := a.Source
		source := serviceaccess.Source{App: q.App, Service: q.Service, ComponentType: q.ComponentType, Stream: q.Stream, Device: q.Device}
		local, e := source.Resolve(scope.Device)
		if e != nil || local.Device != scope.Device || seen[local] || !subscribed(reader, source) {
			return false
		}
		seen[local] = true
		b := m.bindings[bindingKey(source.App, source.Service)]
		if !m.sourceAuthorityCurrentLocked(scope, source, b, w) || !bytes.Equal(a.SignedDelegationJson, b.delegation) {
			return false
		}
	}
	return true
}

// Keep conversion failures explicit even if the physical provider accepts a
// wider time range than the nanosecond app wire representation.
func physicalWireTime(t time.Time) bool {
	return t.UnixNano() > 0 && time.Unix(0, t.UnixNano()).Equal(t)
}
