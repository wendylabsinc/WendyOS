package agentmodule

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"sort"
)

func subscribed(b *binding, source serviceaccess.Source) bool {
	resolved, err := source.Resolve(b.scope.Device)
	if err != nil {
		return false
	}
	for _, s := range b.subscribe {
		allowed, err := s.Resolve(b.scope.Device)
		if err == nil && allowed == resolved {
			return true
		}
	}
	return false
}
func publishes(scope serviceorigin.Scope, p serviceorigin.PublishScope) bool {
	for _, s := range scope.Publish {
		if s == p {
			return true
		}
	}
	return false
}
func (m *Manager) owner(source serviceaccess.Source) serviceevidence.Owner {
	device := source.Device
	if device == "" {
		device = m.cfg.Policy.Device
	}
	return serviceevidence.Owner{Tenant: m.cfg.Policy.Tenant, Site: m.cfg.Policy.Site, Device: device, App: source.App, Service: source.Service, Component: source.ComponentType, Stream: source.Stream}
}

// Every read/write is serialized with listener replacement and identity reset.
// Neither a previous successful RPC nor possession of a digest grants access.
func (m *Manager) access(ctx context.Context, scope serviceorigin.Scope, fence *appservice.Fence, allowed func(*binding) bool, run func(context.Context) error) error {
	return m.accessFenced(ctx, scope, fence, allowed, run, m.withCurrent)
}

// Only the two clock readers use this path. It keeps the same broker, policy,
// binding and identity checks, with an optional shared identity read fence.
// The callback samples clocks and constructs a reply; it must not call any
// journal, archive, evidence or service-lifecycle operation.
func (m *Manager) accessClock(ctx context.Context, scope serviceorigin.Scope, fence *appservice.Fence, run func(context.Context) error) error {
	return m.accessFenced(ctx, scope, fence, func(*binding) bool { return true }, run, m.withCurrentClock)
}

func (m *Manager) accessFenced(ctx context.Context, scope serviceorigin.Scope, fence *appservice.Fence, allowed func(*binding) bool, run func(context.Context) error, enter func(context.Context, uint64, func() error) error) error {
	if fence == nil || m.policyCurrent(ctx) != nil {
		return ErrDenied
	}
	m.mu.RLock()
	b := m.bindings[bindingKey(scope.App, scope.Service)]
	var token uint64
	if !m.closed && b != nil && equalScopes(scope, b.scope) && allowed(b) {
		token = b.token
	}
	m.mu.RUnlock()
	if token == 0 {
		return ErrDenied
	}
	return fence.Commit(ctx, func(ctx context.Context) error {
		return enter(ctx, token, func() error {
			if m.policyCurrent(ctx) != nil {
				return ErrDenied
			}
			m.mu.RLock()
			current := m.bindings[bindingKey(scope.App, scope.Service)]
			valid := !m.closed && current == b && b.token == token && allowed(b)
			m.mu.RUnlock()
			if !valid {
				return ErrDenied
			}
			return m.identityOperation(ctx, token, run)
		})
	})
}
func (m *Manager) query(ctx context.Context, scope serviceorigin.Scope, q serviceaccess.Query, fence *appservice.Fence) (serviceaccess.Page, error) {
	var page serviceaccess.Page
	if q.Validate() != nil {
		return page, ErrDenied
	}
	wanted := map[serviceaccess.Source]serviceaccess.Source{}
	selected := make([]proofref.Source, 0, len(q.Sources))
	remote := map[string]bool{}
	for _, source := range q.Sources {
		resolved, err := source.Resolve(scope.Device)
		if err != nil || resolved.Device != scope.Device && (m.cfg.Replication == nil || !m.cfg.Replication.imports(m.cfg.Policy, resolved)) {
			return page, ErrDenied
		}
		if _, exists := wanted[resolved]; exists {
			return page, ErrDenied
		}
		wanted[resolved] = source
		if resolved.Device == scope.Device {
			selected = append(selected, proofref.Source{Tenant: scope.Tenant, Site: scope.Site, Device: resolved.Device, App: resolved.App, Service: resolved.Service, Component: resolved.ComponentType, Stream: resolved.Stream})
		} else {
			remote[resolved.Device] = true
		}
	}
	allowed := func(b *binding) bool {
		for _, s := range q.Sources {
			if !subscribed(b, s) {
				return false
			}
		}
		return true
	}
	err := m.access(ctx, scope, fence, allowed, func(ctx context.Context) error {
		// Select before proof/schema/media work, under the same ordinary data
		// fence. Current observations and their final lifetimes remain required;
		// actor-only authority metadata is not a substitute for this snapshot.
		var proofs []serviceorigin.SignedRecord
		var originals [][]byte
		collect := func(ctx context.Context) error {
			if len(selected) != 0 {
				var e error
				proofs, e = m.journal.SnapshotSelected(ctx, selected)
				if e != nil {
					return e
				}
				proofs, e = m.filterCurrent(ctx, proofs)
				if e != nil {
					return e
				}
				for _, proof := range proofs {
					raw, e := json.Marshal(proof)
					if e != nil {
						return e
					}
					originals = append(originals, raw)
				}
			}
			for _, device := range m.ReplicaDevices() {
				if !remote[device] {
					continue
				}
				values, e := m.replica.peers[device].store.Snapshot(ctx)
				if e != nil {
					return e
				}
				for _, value := range values {
					originals = append(originals, value.Envelope)
				}
			}
			return nil
		}
		var e error
		// A selected local result may depend on remote raw observations. Take
		// the replica fence before the journal lock, including local-only reads.
		if m.replica == nil {
			e = collect(ctx)
		} else {
			e = m.withReplica(ctx, collect)
		}
		if e != nil {
			return e
		}
		type entry struct {
			key  string
			data []byte
		}
		var matches []entry
		after := ""
		if q.After != "" {
			after, e = serviceaccess.DecodeCursor(q.After)
			if e != nil {
				return e
			}
		}
		for _, raw := range originals {
			var proof serviceorigin.SignedRecord
			if json.Unmarshal(raw, &proof) != nil {
				return ErrDenied
			}
			var record serviceorigin.Record
			// Snapshot already verified these exact signed bytes. Decode solely
			// to filter; return the original envelope rather than reconstructing it.
			if jws.DecodeStrict(proof.Record, &record) != nil {
				return ErrDenied
			}
			origin := serviceaccess.Source{Device: record.Scope.Device, App: record.Scope.App, Service: record.Scope.Service, ComponentType: record.Publication.ComponentType, Stream: record.Publication.Stream}
			source, requested := wanted[origin]
			if record.Scope.Tenant != scope.Tenant || record.Scope.Site != scope.Site || !requested {
				continue
			}
			key := serviceaccess.Key(source, record.Publication.Entity)
			if key <= after {
				continue
			}
			matches = append(matches, entry{key, raw})
		}
		sort.Slice(matches, func(i, j int) bool { return matches[i].key < matches[j].key })
		size := 0
		last := ""
		for _, v := range matches {
			if len(page.SignedRecordsJSON) >= int(q.Limit) || size+len(v.data) > serviceaccess.MaxQueryBytes {
				if last == "" {
					return ErrDenied
				}
				page.Next = serviceaccess.Cursor(last)
				break
			}
			page.SignedRecordsJSON = append(page.SignedRecordsJSON, v.data)
			size += len(v.data)
			last = v.key
		}
		return nil
	})
	if err != nil {
		return serviceaccess.Page{}, err
	}
	return page, nil
}
func (m *Manager) putEvidence(ctx context.Context, scope serviceorigin.Scope, p serviceorigin.PublishScope, q serviceevidence.PutRequest, fence *appservice.Fence) (serviceevidence.PutResponse, error) {
	var out serviceevidence.PutResponse
	if m.evidence == nil {
		return out, ErrDenied
	}
	owner := m.owner(serviceaccess.Source{App: scope.App, Service: scope.Service, ComponentType: p.ComponentType, Stream: p.Stream})
	err := m.access(ctx, scope, fence, func(b *binding) bool { return publishes(b.scope, p) }, func(ctx context.Context) error { var e error; out, e = m.evidence.Put(ctx, owner, q); return e })
	if err != nil {
		return serviceevidence.PutResponse{}, err
	}
	return out, nil
}
func (m *Manager) getEvidence(ctx context.Context, scope serviceorigin.Scope, source serviceaccess.Source, q serviceevidence.GetRequest, fence *appservice.Fence) (serviceevidence.GetResponse, error) {
	var out serviceevidence.GetResponse
	resolved, resolveErr := source.Resolve(scope.Device)
	if resolveErr != nil {
		return out, ErrDenied
	}
	if resolved.Device != scope.Device {
		return m.getRemoteEvidence(ctx, scope, resolved, q, fence)
	}
	if m.evidence == nil {
		return out, ErrDenied
	}
	err := m.access(ctx, scope, fence, func(b *binding) bool { return subscribed(b, source) }, func(ctx context.Context) error {
		var e error
		out, e = m.evidence.Get(ctx, m.owner(source), q)
		return e
	})
	if err != nil {
		return serviceevidence.GetResponse{}, err
	}
	return out, nil
}

// Historical access checks only the current reader grant and retained exact
// source bytes. It deliberately does not grant the old publisher live authority.
func (m *Manager) getRecordProof(ctx context.Context, scope serviceorigin.Scope, ref proofref.Reference, fence *appservice.Fence) (serviceaccess.RecordProof, error) {
	var out serviceaccess.RecordProof
	if m.archive == nil || ref.Validate() != nil || ref.Key.Tenant != scope.Tenant || ref.Key.Site != scope.Site {
		return out, ErrDenied
	}
	source := sourceSelector(ref)
	if source.Device != scope.Device && (m.cfg.Replication == nil || !m.cfg.Replication.imports(m.cfg.Policy, source)) {
		return out, ErrDenied
	}
	expected := ref.Source()
	err := m.access(ctx, scope, fence, func(b *binding) bool { return subscribed(b, source) }, func(ctx context.Context) error {
		proof, e := m.archive.Get(ctx, expected, ref)
		if e != nil {
			return e
		}
		out = serviceaccess.RecordProof{Envelope: proof.Envelope, RetainUntilUnixMS: proof.RetainUntilUnixMS}
		return nil
	})
	if err != nil {
		return serviceaccess.RecordProof{}, err
	}
	return out, nil
}
