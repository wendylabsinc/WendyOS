package agentmodule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replicastore"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (m *Manager) openReplication(ctx context.Context) error {
	if m.cfg.Replication == nil {
		return nil
	}
	rctx, cancel := context.WithCancel(m.ctx)
	r := &replicaState{ctx: rctx, cancel: cancel, peers: map[string]*replicaPeerState{}, latestWake: make(chan struct{}, 1)}
	m.replica = r
	for _, peer := range m.cfg.Replication.Peers {
		r.peers[peer.Device] = &replicaPeerState{policy: peer, latest: newReplicaLatestQueue()}
	}
	ac := sourceauthority.Config{Path: m.cfg.Replication.AuthorityPath, Tenant: m.cfg.Policy.Tenant, Site: m.cfg.Policy.Site, ExecutionDomain: m.cfg.Policy.ExecutionDomain, SampleClockDomain: m.cfg.Policy.SampleClockDomain, Clock: m.deps.Clock, Certificates: m.certificates, AuthorizeLease: m.replicaLeaseAllowed, AuthorizeFloor: m.replicaFloorAllowed, MaxSources: 256, MaxBytes: 16 << 20}
	var err error
	if m.cfg.CreateReplication {
		r.authority, err = sourceauthority.Create(ac)
	} else {
		r.authority, err = sourceauthority.Open(ac)
	}
	if err != nil {
		return err
	}
	for _, device := range m.ReplicaDevices() {
		peer := r.peers[device]
		cfg := replicastore.Config{Endpoint: m.replicaEndpoint(), Peer: replication.Identity{Device: device, BindingSHA256: peer.policy.CredentialSHA256}, Generation: peer.policy.Generation, Import: peer.policy.Import, Export: peer.policy.Export, Local: m.journal, Authority: r.authority, RemotePath: peer.policy.RemotePath, Replica: world.Config{Tenant: m.cfg.Policy.Tenant, Site: m.cfg.Policy.Site, ClockDomain: m.cfg.Policy.SampleClockDomain, Now: m.deps.SampleNow, MaxFutureSkew: 0, MaxEntries: 256, AllowSynthetic: m.cfg.Policy.ExecutionDomain == "simulation", Frames: m.cfg.Policy.Frames}, Clock: m.deps.Clock, Certificates: m.certificates, MaxEntries: 256, MaxProofBytes: 32 << 20, Guard: m.replicaStoreGuard, Authorize: m.replicaPeerWindow, AdmitSchema: m.admitReplicaSchema}
		if m.cfg.CreateReplication {
			peer.store, err = replicastore.Create(cfg)
		} else {
			peer.store, err = replicastore.Open(cfg)
		}
		if err != nil {
			return err
		}
		if peer.policy.EvidencePath != "" {
			peer.evidence, err = serviceevidence.Open(serviceevidence.Config{Directory: peer.policy.EvidencePath, Tenant: m.cfg.Policy.Tenant, Site: m.cfg.Policy.Site, Device: device, Clock: m.deps.Clock})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *Manager) replicaEndpoint() replication.Endpoint {
	p := m.cfg.Policy
	return replication.Endpoint{Tenant: p.Tenant, Site: p.Site, Device: p.Device, ExecutionDomain: p.ExecutionDomain, SampleClockDomain: p.SampleClockDomain}
}

func (m *Manager) ReplicaDevices() []string {
	if m == nil || m.replica == nil {
		return nil
	}
	devices := make([]string, 0, len(m.replica.peers))
	for device := range m.replica.peers {
		devices = append(devices, device)
	}
	sort.Strings(devices)
	return devices
}

// StartReplication activates initialized stores once under a pinned enrolled
// identity. It does not initialize, dial, launch a scheduler or expose an admin
// endpoint. Retained local leases from an earlier process are revoked first.
func (m *Manager) StartReplication(ctx context.Context, transport ReplicaTransport) error {
	if m == nil {
		return ErrDenied
	}
	if m.replica == nil {
		return nil
	}
	if ctx == nil || transport.IncomingPeer == nil || transport.Dial == nil {
		return ErrDenied
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	r := m.replica
	m.mu.RLock()
	refuse := m.closed || r.started || r.failed.Load()
	m.mu.RUnlock()
	if refuse || m.policyCurrent(ctx) != nil {
		return ErrDenied
	}
	id, err := m.deps.Identity.Snapshot(ctx)
	if err != nil || id.Token == 0 || id.Invalidated == nil || !m.deps.Identity.Current(id.Token) {
		return ErrDenied
	}
	w, err := m.physicalNow()
	if err != nil {
		return err
	}
	device, err := m.replicaCertificate(id.ChainDER, w)
	if err != nil || m.identityAllowed(device) != nil {
		return ErrDenied
	}
	id.ChainDER = cloneChain(id.ChainDER)
	m.mu.Lock()
	r.identity, r.transport = id, transport
	m.mu.Unlock()
	err = m.withCurrent(ctx, id.Token, func() error {
		r.mu.Lock()
		defer r.mu.Unlock()
		states, err := r.authority.Snapshot(ctx)
		if err != nil {
			return err
		}
		for _, state := range states {
			if state.Source.Device != m.cfg.Policy.Device || state.LeaseRevision <= state.ThroughRevision || len(state.Lease) == 0 {
				continue
			}
			if state.Epoch == math.MaxUint64 {
				return ErrDenied
			}
			if _, err := r.authority.IssueFloor(ctx, m.replicaSigner(), state.Source, max(state.MinimumEpoch, state.Epoch+1)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		m.failReplication("startup_floor", err)
		return err
	}
	for _, device := range m.ReplicaDevices() {
		peer := r.peers[device]
		incoming := func(ctx context.Context) (replication.Identity, error) {
			id, err := transport.IncomingPeer(ctx)
			if err != nil || id.Device != peer.policy.Device || id.BindingSHA256 != peer.policy.CredentialSHA256 {
				return replication.Identity{}, ErrDenied
			}
			return id, nil
		}
		dial := func(ctx context.Context, target string) (replication.Connection, error) {
			if target != peer.policy.Device {
				return replication.Connection{}, ErrDenied
			}
			return transport.Dial(ctx, target)
		}
		service, err := replication.New(replication.Config{Endpoint: m.replicaEndpoint(), Store: peer.store, IncomingPeer: incoming, Dial: dial, Authorize: m.replicaAccess, Current: m.replicaCurrent, RemoteAuthority: peer.store.RemoteAuthority, ReadEvidence: m.readReplicaEvidence, AdmissionFailure: observeReplicaAdmissionFailure})
		if err != nil {
			m.failReplication("service_configuration", err)
			return err
		}
		peer.service = service
	}
	m.mu.Lock()
	r.started = true
	m.mu.Unlock()
	go func() {
		select {
		case <-r.ctx.Done():
		case <-id.Invalidated:
			m.failReplication("identity_invalidated", ErrDenied)
		}
	}()
	return nil
}

func (m *Manager) ReplicaService() replicav1.WorldReplicaServiceServer {
	if m == nil || m.replica == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return &replicaDispatcher{m: m}
}

func (m *Manager) replicaAccess(ctx context.Context, peer replication.Identity, direction replication.Direction) (replication.Access, error) {
	r := m.replica
	if r == nil || r.peers[peer.Device] == nil {
		return replication.Access{}, ErrDenied
	}
	p := r.peers[peer.Device].policy
	sources := p.Import
	if direction == replication.Export {
		sources = p.Export
	}
	w, e := m.physicalNow()
	if e != nil {
		return replication.Access{}, e
	}
	window, e := m.replicaPeerWindow(ctx, peer, p.Generation, direction, sources, w)
	if e != nil {
		return replication.Access{}, e
	}
	final, e := m.physicalNow()
	if e != nil || !m.policyWindowCurrent(final) || final.Earliest.Before(window.NotBefore) || !final.Latest.Before(window.NotAfter) || ctx.Err() != nil || r.failed.Load() || !m.deps.Identity.Current(r.identity.Token) {
		return replication.Access{}, ErrDenied
	}
	return replication.Access{Generation: p.Generation, Sources: append([]proofref.Source(nil), sources...)}, nil
}

func (m *Manager) replicaCurrent(ctx context.Context, peer replication.Identity, direction replication.Direction, generation uint64) error {
	access, e := m.replicaAccess(ctx, peer, direction)
	if e != nil || access.Generation != generation {
		return ErrDenied
	}
	return nil
}

func (m *Manager) SyncPeer(ctx context.Context, device string) (replication.Result, error) {
	if ctx == nil || m.replica == nil {
		return replication.Result{}, ErrDenied
	}
	m.mu.RLock()
	peer := m.replica.peers[device]
	valid := !m.closed && m.replica.started && !m.replica.failed.Load() && m.deps.Identity.Current(m.replica.identity.Token)
	m.mu.RUnlock()
	if !valid || peer == nil || peer.service == nil {
		return replication.Result{}, ErrDenied
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.replica.ctx, cancel)
	defer func() { stop(); cancel() }()
	result, err := peer.service.SyncPeer(ctx, device)
	if result.UnknownOutcome {
		m.failReplication("repair_admission", err)
	}
	return result, err
}

func (d *replicaDispatcher) service(ctx context.Context) (*replication.Service, error) {
	m := d.m
	m.mu.RLock()
	ready := m.replica != nil && m.replica.started && !m.closed && !m.replica.failed.Load()
	m.mu.RUnlock()
	if !ready || m.policyCurrent(ctx) != nil {
		return nil, status.Error(codes.PermissionDenied, "replication unavailable")
	}
	identity, e := m.replica.transport.IncomingPeer(ctx)
	if e != nil || m.replicaCurrent(ctx, identity, replication.Export, m.replicaGeneration(identity.Device)) != nil {
		return nil, status.Error(codes.PermissionDenied, "replica peer unavailable")
	}
	return m.replica.peers[identity.Device].service, nil
}
func (m *Manager) replicaGeneration(device string) uint64 {
	if m.replica != nil && m.replica.peers[device] != nil {
		return m.replica.peers[device].policy.Generation
	}
	return 0
}
func (d *replicaDispatcher) Inventory(ctx context.Context, request *replicav1.InventoryRequest) (*replicav1.InventoryReply, error) {
	s, err := d.service(ctx)
	if err != nil {
		return nil, err
	}
	return s.Inventory(ctx, request)
}
func (d *replicaDispatcher) Fetch(ctx context.Context, request *replicav1.FetchRequest) (*replicav1.FetchReply, error) {
	s, err := d.service(ctx)
	if err != nil {
		return nil, err
	}
	return s.Fetch(ctx, request)
}

func (d *replicaDispatcher) FetchEvidence(ctx context.Context, request *replicav1.FetchEvidenceRequest) (*replicav1.FetchEvidenceReply, error) {
	s, err := d.service(ctx)
	if err != nil {
		return nil, err
	}
	return s.FetchEvidence(ctx, request)
}

func (m *Manager) replicaSigner() sourceauthority.Signer {
	return sourceauthority.Signer{ChainDER: m.replica.identity.ChainDER, DeviceKey: m.replica.identity.Key}
}
func (m *Manager) replicaActorExported(app, service string) bool {
	if m.replica != nil {
		for _, peer := range m.replica.peers {
			for _, source := range peer.policy.Export {
				if source.App == app && source.Service == service {
					return true
				}
			}
		}
	}
	return false
}
func (m *Manager) replicaSourceGranted(source sourceauthority.Source) bool {
	if source.Device == m.cfg.Policy.Device {
		return m.replicaActorExported(source.App, source.Service)
	}
	peer := m.replica.peers[source.Device]
	if peer != nil {
		for _, permit := range peer.policy.Import {
			if permit.App == source.App && permit.Service == source.Service {
				return true
			}
		}
	}
	return false
}
func (m *Manager) replicaFloorAllowed(ctx context.Context, c sourceauthority.FloorClaims, id wendycert.DeviceIdentity) error {
	if m.replica == nil || m.replica.failed.Load() || !m.deps.Identity.Current(m.replica.identity.Token) || m.policyCurrent(ctx) != nil || m.replicaIdentityAllowed(id) != nil || id.Principal != c.Source.Device || c.Site != m.cfg.Policy.Site || c.ExecutionDomain != m.cfg.Policy.ExecutionDomain || c.SampleClockDomain != m.cfg.Policy.SampleClockDomain || !m.replicaSourceGranted(c.Source) {
		return ErrDenied
	}
	return nil
}
func (m *Manager) replicaLeaseAllowed(ctx context.Context, c sourceauthority.LeaseClaims, id wendycert.DeviceIdentity) error {
	r := m.replica
	if r == nil || r.failed.Load() || !m.deps.Identity.Current(r.identity.Token) || m.policyCurrent(ctx) != nil || m.replicaIdentityAllowed(id) != nil || id.Principal != c.Scope.Device || c.Scope.Site != m.cfg.Policy.Site || c.Scope.ExecutionDomain != m.cfg.Policy.ExecutionDomain || c.SampleClockDomain != m.cfg.Policy.SampleClockDomain || !m.replicaSourceGranted(sourceauthority.SourceOf(c.Scope)) {
		return ErrDenied
	}
	if c.Scope.Device != m.cfg.Policy.Device {
		peer := r.peers[c.Scope.Device]
		for _, pair := range c.Scope.Publish {
			found := false
			for _, permit := range peer.policy.Import {
				found = found || permit.App == c.Scope.App && permit.Service == c.Scope.Service && permit.Component == pair.ComponentType && permit.Stream == pair.Stream
			}
			if !found {
				return ErrDenied
			}
		}
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b := m.bindings[bindingKey(c.Scope.App, c.Scope.Service)]
	if m.closed || b == nil || !equalScopes(b.scope, c.Scope) || b.token != r.identity.Token || b.epoch != c.Epoch || !m.deps.Identity.Current(b.token) || len(b.delegation) == 0 {
		return ErrDenied
	}
	select {
	case <-b.publisherDone:
		return ErrDenied
	default:
	}
	var original serviceorigin.SignedDelegation
	if json.Unmarshal(b.delegation, &original) != nil {
		return ErrDenied
	}
	digest := sha256.Sum256(original.Claims)
	if hex.EncodeToString(digest[:]) != c.DelegationSHA256 || time.UnixMilli(c.NotAfterUnixMS).After(m.cfg.Policy.NotAfter) {
		return ErrDenied
	}
	return nil
}

// RefreshReplication does bounded local issuance only. The host may schedule
// it every ten seconds; failure stops future native work until a fresh module.
func (m *Manager) RefreshReplication(ctx context.Context) error {
	if m.replica == nil {
		return nil
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	err := m.withReplica(ctx, func(ctx context.Context) error {
		states, e := m.replica.authority.Snapshot(ctx)
		if e != nil {
			return e
		}
		for _, state := range states {
			if state.Source.Device == m.cfg.Policy.Device && state.ThroughRevision != 0 {
				if _, e := m.replica.authority.RefreshFloor(ctx, m.replicaSigner(), state.Source); e != nil {
					return e
				}
			}
		}
		m.mu.RLock()
		var bindings []*binding
		for _, b := range m.bindings {
			if m.replicaActorExported(b.scope.App, b.scope.Service) {
				bindings = append(bindings, b)
			}
		}
		m.mu.RUnlock()
		for _, b := range bindings {
			if e = m.issueReplicaLease(ctx, b); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		m.failReplication("authority_refresh", err)
	}
	return err
}

func (m *Manager) issueReplicaLease(ctx context.Context, b *binding) error {
	var delegation serviceorigin.SignedDelegation
	if len(b.delegation) > sourceauthority.MaxEnvelopeBytes || json.Unmarshal(b.delegation, &delegation) != nil {
		return ErrDenied
	}
	canonical, e := json.Marshal(delegation)
	if e != nil || !bytes.Equal(canonical, b.delegation) {
		return ErrDenied
	}
	w, e := m.physicalNow()
	if e != nil {
		return e
	}
	lifetime := min(sourceauthority.MaxLifetime, m.cfg.Policy.NotAfter.Sub(w.Latest))
	if lifetime <= 0 {
		return ErrDenied
	}
	_, e = m.replica.authority.IssueLease(ctx, m.replicaSigner(), sourceauthority.LeaseRequest{Delegation: delegation, Lifetime: lifetime})
	return e
}

func (m *Manager) issueBindingLease(ctx context.Context, b *binding) error {
	if !m.replicaActorExported(b.scope.App, b.scope.Service) {
		return nil
	}
	m.mu.RLock()
	started := m.replica.started
	m.mu.RUnlock()
	if !started {
		return ErrDenied
	}
	err := m.withReplica(ctx, func(ctx context.Context) error { return m.issueReplicaLease(ctx, b) })
	if err != nil {
		m.failReplication("binding_lease", err)
	}
	return err
}

func (m *Manager) retireReplicaBinding(ctx context.Context, b *binding) error {
	remove := func() {
		m.mu.Lock()
		if m.bindings[bindingKey(b.scope.App, b.scope.Service)] == b {
			delete(m.bindings, bindingKey(b.scope.App, b.scope.Service))
		}
		m.mu.Unlock()
	}
	if m.replica == nil || !m.replica.started {
		remove()
		return nil
	}
	err := m.withReplica(ctx, func(ctx context.Context) error {
		if m.replicaActorExported(b.scope.App, b.scope.Service) && b.epoch != 0 {
			if b.epoch == math.MaxUint64 {
				return ErrDenied
			}
			if _, e := m.replica.authority.IssueFloor(ctx, m.replicaSigner(), sourceauthority.SourceOf(b.scope), b.epoch+1); e != nil {
				return e
			}
		}
		remove()
		return nil
	})
	if err != nil {
		m.failReplication("binding_retirement", err)
		remove()
	}
	return err
}

func (m *Manager) closeReplicaStores() error {
	if m.replica == nil {
		return nil
	}
	m.replica.cancel()
	var err error
	for _, peer := range m.replica.peers {
		if peer.evidence != nil {
			err = errors.Join(err, peer.evidence.Close())
		}
		if peer.store != nil {
			err = errors.Join(err, peer.store.Close())
		}
	}
	if m.replica.authority != nil {
		err = errors.Join(err, m.replica.authority.Close())
	}
	return err
}
