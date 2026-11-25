package agentmodule

import (
	"context"
	"encoding/json"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
)

type remoteReadCandidate struct {
	proof                                    serviceorigin.SignedRecord
	record                                   serviceorigin.Record
	source                                   serviceaccess.Source
	sampleUntil, physicalFrom, physicalUntil time.Time
}

// This is an operation-local read check, not a retained authority cache. Every
// remote original must still be the exact live register in a fresh snapshot.
// The shared identity/remote fence prevents floors, repair, and lifecycle work
// from changing any member while the remaining members are checked.
func (m *Manager) filterQueryCurrent(ctx context.Context, proofs []serviceorigin.SignedRecord, reader serviceorigin.Scope) ([]serviceorigin.SignedRecord, error) {
	var local []serviceorigin.SignedRecord
	wanted := map[string]map[string]serviceorigin.SignedRecord{}
	for _, proof := range proofs {
		var r serviceorigin.Record
		if jws.DecodeStrict(proof.Record, &r) != nil || r.Scope.Tenant != reader.Tenant || r.Scope.Site != reader.Site {
			return nil, ErrDenied
		}
		if r.Scope.Device == reader.Device {
			local = append(local, proof)
			continue
		}
		source := serviceaccess.Source{Device: r.Scope.Device, App: r.Scope.App, Service: r.Scope.Service, ComponentType: r.Publication.ComponentType, Stream: r.Publication.Stream}
		if m.cfg.Replication == nil || !m.cfg.Replication.imports(m.cfg.Policy, source) {
			return nil, ErrDenied
		}
		if wanted[r.Scope.Device] == nil {
			wanted[r.Scope.Device] = map[string]serviceorigin.SignedRecord{}
		}
		raw, e := json.Marshal(proof)
		if e != nil {
			return nil, ErrDenied
		}
		wanted[r.Scope.Device][string(raw)] = proof
	}
	if len(wanted) == 0 {
		return m.filterCurrentForReader(ctx, local, &reader)
	}
	var result []serviceorigin.SignedRecord
	err := m.withReplica(ctx, func(ctx context.Context) error {
		locals, e := m.prepareLocalRead(ctx, local)
		if e != nil {
			return e
		}
		var remotes []remoteReadCandidate
		for _, device := range m.ReplicaDevices() {
			originals := wanted[device]
			if len(originals) == 0 {
				continue
			}
			peer := m.replica.peers[device]
			values, e := peer.store.Snapshot(ctx)
			if e != nil {
				return e
			}
			for _, value := range values {
				proof, found := originals[string(value.Envelope)]
				if !found {
					continue
				}
				var r serviceorigin.Record
				var d serviceorigin.DelegationClaims
				if jws.DecodeStrict(proof.Record, &r) != nil || jws.DecodeStrict(proof.Delegation.Claims, &d) != nil || r.Publication.Tombstone {
					return ErrDenied
				}
				source := serviceaccess.Source{Device: r.Scope.Device, App: r.Scope.App, Service: r.Scope.Service, ComponentType: r.Publication.ComponentType, Stream: r.Publication.Stream}
				lease, e := m.replica.authority.CurrentLease(ctx, sourceauthority.SourceOf(r.Scope))
				if e != nil {
					continue
				}
				var claims sourceauthority.LeaseClaims
				if jws.DecodeStrict(lease.Claims, &claims) != nil || !equalScopes(r.Scope, claims.Scope) || r.Epoch != claims.Epoch || r.DelegationSHA256 != claims.DelegationSHA256 {
					continue
				}
				w, e := m.physicalNow()
				if e != nil {
					return e
				}
				id, e := m.certificates.Verify(proof.Delegation.ChainDER, w.Earliest, w.Latest)
				if e != nil || m.replicaIdentityAllowed(id) != nil {
					continue
				}
				peerWindow, e := m.replicaPeerWindow(ctx, replication.Identity{Device: device, BindingSHA256: peer.policy.CredentialSHA256}, peer.policy.Generation, replication.Import, peer.policy.Import, w)
				if e != nil {
					return e
				}
				deadline, e := staticSampleDeadline((serviceorigin.VerifiedRecord{Record: r}).Component())
				if e != nil {
					continue
				}
				from := maxTime(time.UnixMilli(r.IssuedAtUnixMS), time.UnixMilli(d.IssuedAtUnixMS), time.UnixMilli(claims.NotBeforeUnixMS), peerWindow.NotBefore)
				until := earlier(earlier(time.UnixMilli(r.ExpiresAtUnixMS), time.UnixMilli(d.ExpiresAtUnixMS)), earlier(time.UnixMilli(claims.NotAfterUnixMS), earlier(id.ValidUntil, peerWindow.NotAfter)))
				interpretationUntil, e := replication.RecordInterpretationUntil(r)
				if e != nil {
					continue
				}
				if !interpretationUntil.IsZero() {
					until = earlier(until, interpretationUntil)
				}
				remotes = append(remotes, remoteReadCandidate{proof: proof, record: r, source: source, sampleUntil: earlier(deadline, time.Unix(0, value.Descriptor.ExpiresAtUnixNS)), physicalFrom: from, physicalUntil: until})
			}
		}
		if m.policyCurrent(ctx) != nil {
			return ErrDenied
		}
		sample := m.sampleNow(ctx)
		physical, e := m.physicalNow()
		if e != nil || sample.IsZero() {
			return ErrDenied
		}
		m.mu.RLock()
		defer m.mu.RUnlock()
		if ctx.Err() != nil || m.replica.failed.Load() || !m.readerCurrentLocked(reader, physical) {
			return ErrDenied
		}
		binding := m.bindings[bindingKey(reader.App, reader.Service)]
		live := map[string]bool{}
		for _, item := range locals {
			if item.derived != nil {
				if !m.derivedCurrentLocked(ctx, *item.derived, sample, physical) {
					continue
				}
			} else if !m.recordCurrentLocked(item.record, item.sampleUntil, item.physicalUntil, sample, physical) {
				continue
			}
			live[string(item.proof.Record)] = true
		}
		for _, item := range remotes {
			if !subscribed(binding, item.source) || item.record.Publication.Timing.SampleTime.After(sample) || !sample.Before(item.sampleUntil) || physical.Earliest.Before(item.physicalFrom) || !physical.Latest.Before(item.physicalUntil) {
				continue
			}
			live[string(item.proof.Record)] = true
		}
		for _, proof := range proofs {
			if live[string(proof.Record)] {
				result = append(result, proof)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func maxTime(values ...time.Time) time.Time {
	var out time.Time
	for _, t := range values {
		if t.After(out) {
			out = t
		}
	}
	return out
}
