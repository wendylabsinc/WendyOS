package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"google.golang.org/protobuf/proto"
	"time"
)

// Resolve proof dependencies before taking one final clock pair and binding
// lock. Work on a later result cannot keep an earlier source/result live.
func (m *Manager) filterCurrent(ctx context.Context, proofs []serviceorigin.SignedRecord) ([]serviceorigin.SignedRecord, error) {
	return m.filterCurrentForReader(ctx, proofs, nil)
}

type localReadCandidate struct {
	proof                      serviceorigin.SignedRecord
	record                     serviceorigin.Record
	sampleUntil, physicalUntil time.Time
	derived                    *derivedState
}

func (m *Manager) prepareLocalRead(ctx context.Context, proofs []serviceorigin.SignedRecord) ([]localReadCandidate, error) {
	items := make([]localReadCandidate, 0, len(proofs))
	for _, proof := range proofs {
		var r serviceorigin.Record
		if jws.DecodeStrict(proof.Record, &r) != nil {
			return nil, ErrDenied
		}
		v := serviceorigin.VerifiedRecord{Record: r}
		sampleUntil, e := staticSampleDeadline(v.Component())
		if e != nil {
			continue
		}
		item := localReadCandidate{proof: proof, record: r, sampleUntil: sampleUntil, physicalUntil: time.UnixMilli(r.ExpiresAtUnixMS)}
		if !r.Publication.Tombstone {
			switch r.Publication.ComponentType {
			case sensorrecord.ComponentType:
				var physical time.Time
				item.sampleUntil, physical, e = sensorrecord.LiveDeadlines(v.Component())
				if e != nil {
					continue
				}
				item.physicalUntil = earlier(item.physicalUntil, physical)
			case resultrecord.ComponentType:
				verified, e := m.verifyRecord(ctx, proof)
				if e != nil {
					continue
				}
				raw, e := json.Marshal(proof)
				if e != nil {
					return nil, e
				}
				state, e := m.retainedResult(ctx, raw, verified)
				if e != nil {
					continue
				}
				item.derived = &state
			}
		}
		items = append(items, item)
	}
	return items, nil
}

// A local Snapshot has no socket reader. An app response must provide its
// exact reader so the final group interval also checks that reader's authority.
func (m *Manager) filterCurrentForReader(ctx context.Context, proofs []serviceorigin.SignedRecord, reader *serviceorigin.Scope) ([]serviceorigin.SignedRecord, error) {
	if m.needsReplicaForRead(proofs) && m.remoteOperation(ctx) == nil {
		var out []serviceorigin.SignedRecord
		err := m.withReplica(ctx, func(ctx context.Context) error {
			var e error
			out, e = m.filterCurrentForReader(ctx, proofs, reader)
			return e
		})
		return out, err
	}
	items, e := m.prepareLocalRead(ctx, proofs)
	if e != nil {
		return nil, e
	}
	if m.policyCurrent(ctx) != nil {
		return nil, ErrDenied
	}
	sampleNow := m.sampleNow(ctx)
	if sampleNow.IsZero() {
		return nil, ErrDenied
	}
	window, e := m.physicalNow()
	if e != nil || window.Validate() != nil {
		return nil, ErrDenied
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed || m.policyFailed.Load() || ctx.Err() != nil || !m.policyWindowCurrent(window) || reader != nil && !m.readerCurrentLocked(*reader, window) {
		return nil, ErrDenied
	}
	var result []serviceorigin.SignedRecord
	for _, item := range items {
		if item.derived != nil {
			if !m.derivedCurrentLocked(ctx, *item.derived, sampleNow, window) {
				continue
			}
		} else if !m.recordCurrentLocked(item.record, item.sampleUntil, item.physicalUntil, sampleNow, window) {
			continue
		}
		result = append(result, item.proof)
	}
	return result, nil
}

func (m *Manager) finalizeResponse(ctx context.Context, scope serviceorigin.Scope, response proto.Message) (proto.Message, error) {
	if m.policyCurrent(ctx) != nil {
		return nil, ErrDenied
	}
	// The broker has just checked the pinned process. Check its immutable
	// listener binding again after policy I/O, without another peer read.
	m.mu.RLock()
	b := m.bindings[bindingKey(scope.App, scope.Service)]
	valid := !m.closed && b != nil && equalScopes(b.scope, scope) && m.deps.Identity.Current(b.token)
	m.mu.RUnlock()
	if !valid {
		return nil, ErrDenied
	}
	switch out := response.(type) {
	case *appv1.QueryReply:
		proofs := make([]serviceorigin.SignedRecord, 0, len(out.SignedRecordJson))
		original := map[string][]byte{}
		for _, raw := range out.SignedRecordJson {
			var proof serviceorigin.SignedRecord
			// This is the canonical envelope produced by query above, not a
			// new external admission. Round-trip its fixed shape at the actual
			// page budget; the strict signed inner record has its own 64KiB cap.
			if len(raw) > serviceaccess.MaxQueryBytes || json.Unmarshal(raw, &proof) != nil {
				return nil, ErrDenied
			}
			canonical, e := json.Marshal(proof)
			if e != nil || !bytes.Equal(raw, canonical) {
				return nil, ErrDenied
			}
			proofs = append(proofs, proof)
			original[string(proof.Record)] = raw
		}
		live, e := m.filterQueryCurrent(ctx, proofs, scope)
		if e != nil {
			return nil, e
		}
		result := &appv1.QueryReply{Next: out.Next}
		for _, proof := range live {
			result.SignedRecordJson = append(result.SignedRecordJson, original[string(proof.Record)])
		}
		if !m.bindingCurrent(scope) {
			return nil, ErrDenied
		}
		return result, nil
	case *appv1.GetRecordProofReply:
		if !out.Historical || !m.retentionResponseCurrent(scope, out.RetainUntilUnixMs) {
			return nil, ErrDenied
		}
	case *appv1.GetEvidenceReply:
		if !m.retentionResponseCurrent(scope, out.RetainUntilUnixMs) {
			return nil, ErrDenied
		}
	case *appv1.PutEvidenceReply:
		if !m.retentionResponseCurrent(scope, out.RetainUntilUnixMs) {
			return nil, ErrDenied
		}
	case *appv1.ClockReply:
		if !m.clockResponseCurrent(scope, out) {
			return nil, ErrDenied
		}
	case *appv1.PhysicalClockReply:
		if !m.physicalClockResponseCurrent(scope, out) {
			return nil, ErrDenied
		}
	case *appv1.SourceAuthorityReply:
		if !m.sourceAuthorityResponseCurrent(scope, out) {
			return nil, ErrDenied
		}
	case *appv1.SensorFrameChunk:
		// Native frame bytes describe one snapshot. They carry no observation
		// freshness or UTC capture assertion. The reader's existing authority
		// must still be current after the final policy check.
		window, err := m.physicalNow()
		if err != nil || !m.readerCurrent(scope, window) {
			return nil, ErrDenied
		}
	default:
		return nil, ErrDenied
	}
	if !m.bindingCurrent(scope) {
		return nil, ErrDenied
	}
	return response, nil
}
func (m *Manager) bindingCurrent(scope serviceorigin.Scope) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	b := m.bindings[bindingKey(scope.App, scope.Service)]
	return !m.closed && !m.policyFailed.Load() && b != nil && equalScopes(scope, b.scope) && m.deps.Identity.Current(b.token)
}
func (m *Manager) retentionResponseCurrent(scope serviceorigin.Scope, until int64) bool {
	w, e := m.physicalNow()
	return e == nil && until > w.Latest.UnixMilli() && m.readerCurrent(scope, w)
}

func (m *Manager) policyWindowCurrent(w trustedtime.Interval) bool {
	if w.Validate() != nil || w.Earliest.Before(m.cfg.Policy.NotBefore) || !w.Latest.Before(m.cfg.Policy.NotAfter) {
		m.policyFailed.Store(true)
		return false
	}
	return !m.policyFailed.Load()
}

func (m *Manager) readerCurrent(scope serviceorigin.Scope, w trustedtime.Interval) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.readerCurrentLocked(scope, w)
}

// No clock or policy I/O occurs here. The immutable window was captured from
// this exact publisher at construction and already fits its certificate path.
// It cannot replace current identity/policy/binding checks or extend a lease.
func (m *Manager) readerCurrentLocked(scope serviceorigin.Scope, w trustedtime.Interval) bool {
	if !m.policyWindowCurrent(w) {
		return false
	}
	b := m.bindings[bindingKey(scope.App, scope.Service)]
	return !m.closed && b != nil && equalScopes(b.scope, scope) && m.deps.Identity.Current(b.token) && !b.authorityFrom.IsZero() && b.authorityUntil.After(b.authorityFrom) && !w.Earliest.Before(b.authorityFrom) && w.Latest.Before(b.authorityUntil)
}
