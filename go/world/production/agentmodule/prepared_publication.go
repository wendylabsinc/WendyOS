package agentmodule

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"

	"github.com/wendylabsinc/wendy/go/world/production/proofarchive"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

type preparationKey struct{}
type preparedReport struct {
	inputVerifications, mediaReads, currentChecks int
	closed                                        bool
}
type preparedMedia struct {
	ref    serviceevidence.Reference
	handle *serviceevidence.PreparedRead
	store  *serviceevidence.Store
}

// Only admitArchived creates this operation. No handle is exported, reused by
// another Publish, or installed in manager state. Its callbacks are synchronous
// within that admission; no preparation lock spans archive/journal callbacks.
type preparedPublication struct {
	manager                                       *Manager
	ctx                                           context.Context
	cancel                                        context.CancelFunc
	claim                                         []byte
	state                                         derivedState
	descriptors                                   []proofarchive.Descriptor
	media                                         []preparedMedia
	building, ready, closed                       bool
	inputVerifications, mediaReads, currentChecks int
}

func publicationPreparation(ctx context.Context) (*preparedPublication, bool) {
	if ctx == nil {
		return nil, false
	}
	p, ok := ctx.Value(preparationKey{}).(*preparedPublication)
	return p, ok
}

func (m *Manager) preparePublication(ctx context.Context, raw []byte) (context.Context, *preparedPublication, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, nil, ErrDenied
	}
	if _, present := publicationPreparation(ctx); present {
		return nil, nil, ErrDenied
	}
	child, cancel := context.WithCancel(ctx)
	p := &preparedPublication{manager: m, cancel: cancel, claim: bytes.Clone(raw)}
	p.ctx = context.WithValue(child, preparationKey{}, p)
	return p.ctx, p, nil
}
func (p *preparedPublication) close() {
	if p == nil || p.closed {
		return
	}
	p.closed = true
	p.cancel()
	for _, media := range p.media {
		media.handle.Close()
	}
}

func (p *preparedPublication) matches(ctx context.Context, m *Manager, claim serviceorigin.VerifiedRecord) bool {
	if p == nil || p.manager != m || p.closed || ctx != p.ctx || ctx.Err() != nil {
		return false
	}
	var signed serviceorigin.SignedRecord
	if json.Unmarshal(p.claim, &signed) != nil || !bytes.Equal(signed.Record, claim.Bytes) {
		return false
	}
	// A callback projection must still be the exact record and delegation that
	// its signature verifier returned, not caller-rewritten parsed metadata.
	var record serviceorigin.Record
	var delegation serviceorigin.DelegationClaims
	return json.Unmarshal(signed.Record, &record) == nil && json.Unmarshal(signed.Delegation.Claims, &delegation) == nil &&
		reflect.DeepEqual(record, claim.Record) && reflect.DeepEqual(delegation, claim.Delegation)
}

func (m *Manager) derivedFromDescriptors(ctx context.Context, claim serviceorigin.VerifiedRecord, descriptors []proofarchive.Descriptor) (derivedState, error) {
	p, present := publicationPreparation(ctx)
	if !present {
		return m.buildDerivedFromDescriptors(ctx, claim, descriptors)
	}
	if !p.matches(ctx, m, claim) || p.building {
		return derivedState{}, ErrDenied
	}
	if !p.ready {
		p.building = true
		state, err := m.buildDerivedFromDescriptors(ctx, claim, descriptors)
		p.building = false
		if err != nil {
			p.close()
			return derivedState{}, err
		}
		p.state = state
		p.descriptors = make([]proofarchive.Descriptor, len(descriptors))
		for i, descriptor := range descriptors {
			p.descriptors[i] = descriptor
			p.descriptors[i].Envelope = bytes.Clone(descriptor.Envelope)
		}
		p.ready = true
		return state, nil
	}
	if len(descriptors) != len(p.descriptors) {
		return derivedState{}, ErrDenied
	}
	for i, descriptor := range descriptors {
		original := p.descriptors[i]
		if descriptor.Reference != original.Reference || !bytes.Equal(descriptor.Envelope, original.Envelope) ||
			descriptor.Admitted != original.Admitted || descriptor.RecentUntilUnixMS != original.RecentUntilUnixMS ||
			descriptor.RetainUntilUnixMS < original.RetainUntilUnixMS {
			return derivedState{}, ErrDenied
		}
	}
	return p.current(ctx, m, claim)
}

// Reusing immutable facts never reuses a clock observation or authority. All
// original source/delegation/certificate/media deadlines remain in state, and
// every callback performs fresh policy, source time, physical and binding checks.
func (p *preparedPublication) current(ctx context.Context, m *Manager, claim serviceorigin.VerifiedRecord) (derivedState, error) {
	if !p.matches(ctx, m, claim) || !p.ready || p.building {
		return derivedState{}, ErrDenied
	}
	w, err := m.physicalNow()
	if err != nil {
		return derivedState{}, ErrDenied
	}
	for _, media := range p.media {
		if media.store == nil || media.store.ValidatePrepared(ctx, media.handle, w) != nil {
			return derivedState{}, ErrDenied
		}
	}
	// Store locks above may wait behind disk I/O. The common source/physical
	// observations below must occur afterwards, including for the final reply.
	if m.policyCurrent(ctx) != nil {
		return derivedState{}, ErrDenied
	}
	sampleNow := m.sampleNow(ctx)
	w, err = m.physicalNow()
	if err != nil {
		return derivedState{}, ErrDenied
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.policyWindowCurrent(w) || !m.derivedCurrentLocked(ctx, p.state, sampleNow, w) {
		return derivedState{}, ErrDenied
	}
	p.currentChecks++
	return p.state, nil
}

func (m *Manager) readInputMedia(ctx context.Context, owner serviceevidence.Owner, sha string, size int64, contentType string) (serviceevidence.Reference, []byte, error) {
	store := m.evidenceStore(owner)
	if store == nil {
		return serviceevidence.Reference{}, nil, ErrDenied
	}
	p, present := publicationPreparation(ctx)
	if !present {
		return store.ReadComplete(ctx, owner, sha, size, contentType)
	}
	if p.manager != m || p.ctx != ctx || p.closed || !p.building || ctx.Err() != nil {
		return serviceevidence.Reference{}, nil, ErrDenied
	}
	// Each raw leaf has one media object. Multiple leaves naming the same exact
	// bytes are allowed, but still validated independently during preparation.
	handle, ref, data, err := store.PrepareRead(ctx, owner, sha, size, contentType)
	if err != nil {
		return serviceevidence.Reference{}, nil, err
	}
	p.media = append(p.media, preparedMedia{ref, handle, store})
	p.mediaReads++
	return ref, data, nil
}
func (m *Manager) matchSchemaMedia(ctx context.Context, owner serviceevidence.Owner, sha string, size int64, contentType string) (serviceevidence.Reference, error) {
	if _, present := publicationPreparation(ctx); !present {
		ref, _, err := m.readInputMedia(ctx, owner, sha, size, contentType)
		return ref, err
	}
	ref, _, err := m.readInputMedia(ctx, owner, sha, size, contentType)
	return ref, err
}
func (m *Manager) matchInputMedia(ctx context.Context, owner serviceevidence.Owner, sha string, size int64, contentType string) (serviceevidence.Reference, error) {
	p, present := publicationPreparation(ctx)
	if !present {
		ref, _, err := m.readInputMedia(ctx, owner, sha, size, contentType)
		return ref, err
	}
	if p.manager != m || p.ctx != ctx || p.closed || !p.building || ctx.Err() != nil {
		return serviceevidence.Reference{}, ErrDenied
	}
	for _, media := range p.media {
		if media.ref.Owner == owner && media.ref.SHA256 == sha && media.ref.Size == size && media.ref.ContentType == contentType {
			return media.ref, nil
		}
	}
	return serviceevidence.Reference{}, ErrDenied
}
