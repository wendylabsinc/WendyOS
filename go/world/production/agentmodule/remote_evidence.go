package agentmodule

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// Caches retain the original owner. They contain media, never a delegated
// publisher identity. A historical read does not make its observation live.
func (m *Manager) evidenceStore(owner serviceevidence.Owner) *serviceevidence.Store {
	if owner.Tenant != m.cfg.Policy.Tenant || owner.Site != m.cfg.Policy.Site {
		return nil
	}
	if owner.Device == m.cfg.Policy.Device {
		return m.evidence
	}
	if m.replica == nil || owner.Component != sensorrecord.ComponentType {
		return nil
	}
	source := serviceaccess.Source{Device: owner.Device, App: owner.App, Service: owner.Service, ComponentType: owner.Component, Stream: owner.Stream}
	if !m.cfg.Replication.imports(m.cfg.Policy, source) {
		return nil
	}
	peer := m.replica.peers[owner.Device]
	if peer == nil {
		return nil
	}
	return peer.evidence
}

func evidenceSource(owner serviceevidence.Owner) proofref.Source {
	return proofref.Source{Tenant: owner.Tenant, Site: owner.Site, Device: owner.Device, App: owner.App, Service: owner.Service, Component: owner.Component, Stream: owner.Stream}
}

// This callback only exports local original bytes. No cache or third-origin
// forwarding is reachable through the native evidence RPC.
func (m *Manager) readReplicaEvidence(ctx context.Context, peer replication.Identity, generation uint64, source proofref.Source, request serviceevidence.GetRequest) (out serviceevidence.GetResponse, err error) {
	if m.evidence == nil || source.Device != m.cfg.Policy.Device || source.Tenant != m.cfg.Policy.Tenant || source.Site != m.cfg.Policy.Site || source.Component != sensorrecord.ComponentType {
		return out, ErrDenied
	}
	err = m.withReplica(ctx, func(ctx context.Context) error {
		if m.replicaCurrent(ctx, peer, replication.Export, generation) != nil {
			return ErrDenied
		}
		allowed := false
		for _, grant := range m.replica.peers[peer.Device].policy.Export {
			allowed = allowed || source == grant
		}
		if !allowed {
			return ErrDenied
		}
		owner := serviceevidence.Owner{Tenant: source.Tenant, Site: source.Site, Device: source.Device, App: source.App, Service: source.Service, Component: source.Component, Stream: source.Stream}
		var e error
		out, e = m.evidence.Get(ctx, owner, request)
		if e != nil {
			return e
		}
		return m.replicaCurrent(ctx, peer, replication.Export, generation)
	})
	if err != nil {
		return serviceevidence.GetResponse{}, err
	}
	return out, nil
}

// Preflight is read-only. The actual broker fence and identity lock are taken
// once, after any network I/O, and check this exact reader binding again.
func (m *Manager) remoteEvidenceReader(ctx context.Context, scope serviceorigin.Scope, source serviceaccess.Source) (*binding, error) {
	if ctx == nil || ctx.Err() != nil || m.policyCurrent(ctx) != nil || m.evidenceStore(m.owner(source)) == nil {
		return nil, ErrDenied
	}
	w, err := m.physicalNow()
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b := m.bindings[bindingKey(scope.App, scope.Service)]
	if !m.readerCurrentLocked(scope, w) || b == nil || !subscribed(b, source) {
		return nil, ErrDenied
	}
	return b, nil
}

func (m *Manager) getRemoteEvidence(ctx context.Context, scope serviceorigin.Scope, source serviceaccess.Source, request serviceevidence.GetRequest, fence *appservice.Fence) (out serviceevidence.GetResponse, err error) {
	if fence == nil || request.Offset < 0 || request.Length < 1 || request.Length > serviceevidence.MaxChunk {
		return out, ErrDenied
	}
	b, err := m.remoteEvidenceReader(ctx, scope, source)
	if err != nil {
		return out, err
	}
	owner := m.owner(source)
	store := m.evidenceStore(owner)
	// This cache probe holds only the evidence store's own lock. Both hits and
	// misses pass through the final reader fence before bytes are returned.
	_, cached := store.Get(ctx, owner, request)
	var ref serviceevidence.Reference
	var data []byte
	if cached != nil {
		if !errors.Is(cached, serviceevidence.ErrUnavailable) {
			return out, cached
		}
		ref, data, err = m.fetchRemoteEvidence(ctx, scope, source, request.SHA256, b)
		if err != nil {
			return out, err
		}
		if request.Offset > ref.Size {
			return out, ErrDenied
		}
	}
	err = m.access(ctx, scope, fence, func(current *binding) bool { return current == b && subscribed(current, source) }, func(ctx context.Context) error {
		if len(data) != 0 {
			if e := retainRemoteEvidence(ctx, store, ref, data); e != nil {
				return e
			}
		}
		var e error
		out, e = store.Get(ctx, owner, request)
		return e
	})
	if err != nil {
		return serviceevidence.GetResponse{}, err
	}
	return out, nil
}

func retainRemoteEvidence(ctx context.Context, store *serviceevidence.Store, ref serviceevidence.Reference, data []byte) error {
	if int64(len(data)) != ref.Size || len(data) == 0 {
		return ErrDenied
	}
	for offset := 0; offset < len(data); offset += serviceevidence.MaxChunk {
		end := min(offset+serviceevidence.MaxChunk, len(data))
		put, err := store.Put(ctx, ref.Owner, serviceevidence.PutRequest{SHA256: ref.SHA256, Size: ref.Size, Offset: int64(offset), Data: data[offset:end], ContentType: ref.ContentType, RetainUntilUnixMS: ref.RetainUntilUnixMS})
		if err != nil {
			return err
		}
		if put.Reference != ref || put.Received < int64(end) || end == len(data) && !put.Complete {
			return ErrDenied
		}
	}
	return nil
}

// Fetch one complete object so the cache can validate its digest before any
// ordinary app consumes it. Native calls happen outside all host operation,
// identity, replica, journal and archive locks. Each range checks current peer
// import/export policy through the existing authenticated transport.
func (m *Manager) fetchRemoteEvidence(ctx context.Context, scope serviceorigin.Scope, source serviceaccess.Source, digest string, reader *binding) (ref serviceevidence.Reference, data []byte, err error) {
	if ctx == nil || ctx.Value(replicaOperationKey{}) != nil || m.replica == nil {
		return ref, nil, ErrDenied
	}
	m.mu.RLock()
	peer := m.replica.peers[source.Device]
	ready := !m.closed && m.replica.started && !m.replica.failed.Load() && m.deps.Identity.Current(m.replica.identity.Token)
	m.mu.RUnlock()
	if !ready || peer == nil || peer.service == nil || peer.evidence == nil {
		return ref, nil, ErrDenied
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.replica.ctx, cancel)
	defer func() { stop(); cancel() }()
	owner := m.owner(source)
	for {
		current, e := m.remoteEvidenceReader(ctx, scope, source)
		if e != nil || current != reader {
			return serviceevidence.Reference{}, nil, ErrDenied
		}
		got, e := peer.service.ReadPeerEvidence(ctx, source.Device, evidenceSource(owner), serviceevidence.GetRequest{SHA256: digest, Offset: int64(len(data)), Length: serviceevidence.MaxChunk})
		if e != nil {
			return serviceevidence.Reference{}, nil, e
		}
		if got.Size < 1 || got.Size > replication.MaxEvidenceBytes || got.Owner != owner || got.SHA256 != digest || got.Offset != int64(len(data)) || len(got.Data) == 0 {
			return serviceevidence.Reference{}, nil, ErrDenied
		}
		if len(data) == 0 {
			ref = got.Reference
			data = make([]byte, 0, ref.Size)
		}
		if got.Reference != ref || int64(len(got.Data)) != min(int64(serviceevidence.MaxChunk), ref.Size-int64(len(data))) {
			return serviceevidence.Reference{}, nil, ErrDenied
		}
		data = append(data, got.Data...)
		if int64(len(data)) == ref.Size {
			break
		}
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != ref.SHA256 {
		return serviceevidence.Reference{}, nil, ErrDenied
	}
	return ref, data, nil
}

// Remote capture admission verified its original envelope and current source.
// A local derived claim additionally validates its already-cached exact media,
// using the same operation-scoped preparation as local inputs.
func (m *Manager) validateRemoteInputMedia(ctx context.Context, v serviceorigin.VerifiedRecord) error {
	if m.remoteOperation(ctx) == nil {
		return ErrDenied
	}
	capture, err := sensorrecord.FromComponent(v.Component())
	if err != nil {
		return err
	}
	ref, err := capture.Evidence()
	if err != nil || ref == nil {
		return err
	}
	owner := m.owner(sourceSelector(proofref.Reference{Key: v.Component().Key}))
	_, data, err := m.readInputMedia(ctx, owner, ref.SHA256, ref.Size, ref.ContentType)
	if err != nil {
		return err
	}
	return sensorrecord.ValidateMedia(capture, data)
}
