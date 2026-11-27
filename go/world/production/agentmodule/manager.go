package agentmodule

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofarchive"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceevidence"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type binding struct {
	scope                         serviceorigin.Scope
	subscribe                     []serviceaccess.Source
	token, epoch                  uint64
	authorityFrom, authorityUntil time.Time
	mount                         appservice.Mount
	delegation                    []byte
	delegationClaims              serviceorigin.DelegationClaims
	publisherDone                 <-chan struct{}
	device                        wendycert.DeviceIdentity
}
type Manager struct {
	cfg              Config
	deps             Dependencies
	ctx              context.Context
	cancel           context.CancelFunc
	lifecycle        sync.Mutex
	mu               sync.RWMutex
	bindings         map[string]*binding
	closed           bool
	policyFailed     atomic.Bool
	clockMu          sync.Mutex
	clockSeen        bool
	clockLast        trustedtime.Interval
	clockIncarnation string
	broker           *appservice.Manager
	journal          *servicejournal.Journal
	archive          *proofarchive.Archive
	verifier         *serviceorigin.Verifier
	remoteVerifier   *serviceorigin.Verifier
	certificates     *wendycert.DeviceVerifier
	evidence         *serviceevidence.Store
	replica          *replicaState
	nativeCamera     NativeCameraProvider
}

func bindingKey(app, service string) string { return app + "\x00" + service }

func New(ctx context.Context, cfg Config, deps Dependencies) (*Manager, error) {
	if ctx == nil || deps.Identity == nil || deps.Clock == nil || deps.SampleNow == nil || deps.PeerCheck == nil || deps.PolicyCurrent == nil || cfg.Policy.validateWithReplication(cfg.Replication) != nil || cfg.Replication.validate(cfg.Policy, cfg.JournalPath, cfg.ProofArchivePath, cfg.EvidenceDirectory) != nil || cfg.CreateReplication && cfg.Replication == nil || appservice.ValidateLimits(cfg.Limits) != nil || validateNativeCameras(cfg.Policy, cfg.NativeCameras) != nil {
		return nil, ErrDenied
	}
	// Own all policy slices; callers cannot mutate live grants after validation.
	data, err := json.Marshal(cfg.Policy)
	if err != nil {
		return nil, err
	}
	var ownedPolicy Policy
	if err = json.Unmarshal(data, &ownedPolicy); err != nil {
		return nil, err
	}
	cfg.Policy = ownedPolicy
	cfg.NativeCameras = append([]NativeCameraBinding(nil), cfg.NativeCameras...)
	cfg.Roots = cloneChain(cfg.Roots)
	if cfg.Replication != nil {
		data, err := json.Marshal(cfg.Replication)
		if err != nil {
			return nil, err
		}
		cfg.Replication = &ReplicaConfig{}
		if err = json.Unmarshal(data, cfg.Replication); err != nil {
			return nil, err
		}
	}
	if cfg.CaptureSource != nil {
		copy := *cfg.CaptureSource
		cfg.CaptureSource = &copy
	}
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{cfg: cfg, deps: deps, ctx: ctx, cancel: cancel, bindings: map[string]*binding{}}
	var incarnation [16]byte
	rand.Read(incarnation[:])
	m.clockIncarnation = hex.EncodeToString(incarnation[:])
	ok := false
	defer func() {
		if !ok {
			m.Close()
		}
	}()
	if err = m.policyCurrent(ctx); err != nil {
		return nil, err
	}
	m.certificates, err = wendycert.NewDevice(wendycert.Config{Tenant: cfg.Policy.Tenant, Roots: cfg.Roots})
	if err != nil {
		return nil, err
	}
	verifier, err := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: cfg.Policy.Tenant, Site: cfg.Policy.Site, ExecutionDomain: cfg.Policy.ExecutionDomain, Certificates: m.certificates, Clock: deps.Clock, Authorize: m.authorize})
	if err != nil {
		return nil, err
	}
	m.verifier = verifier
	if cfg.Replication != nil {
		m.remoteVerifier, err = serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: cfg.Policy.Tenant, Site: cfg.Policy.Site, ExecutionDomain: cfg.Policy.ExecutionDomain, Certificates: m.certificates, Clock: deps.Clock, Authorize: m.authorizeRemoteDelegation})
		if err != nil {
			return nil, err
		}
	}
	if cfg.CreateProofArchive && cfg.ProofArchivePath == "" {
		return nil, ErrDenied
	}
	if cfg.EvidenceDirectory != "" {
		m.evidence, err = serviceevidence.Open(serviceevidence.Config{Directory: cfg.EvidenceDirectory, Tenant: cfg.Policy.Tenant, Site: cfg.Policy.Site, Device: cfg.Policy.Device, Clock: deps.Clock})
		if err != nil {
			return nil, err
		}
	}
	jc := servicejournal.Config{Path: cfg.JournalPath, Replica: world.Config{Tenant: cfg.Policy.Tenant, Site: cfg.Policy.Site, ClockDomain: cfg.Policy.SampleClockDomain, Now: deps.SampleNow, MaxFutureSkew: 0, MaxEntries: 256, AllowSynthetic: cfg.Policy.ExecutionDomain == "simulation", Frames: cfg.Policy.Frames}, Verifier: verifier, PhysicalClock: deps.Clock, AdmitSchema: m.admitSchema, MaxProofBytes: 32 << 20, MaxEntries: 256}
	if cfg.CreateJournal {
		m.journal, err = servicejournal.Create(jc)
	} else {
		m.journal, err = servicejournal.Open(jc)
	}
	if err != nil {
		return nil, err
	}
	if cfg.ProofArchivePath != "" {
		ac := proofarchive.Config{Path: cfg.ProofArchivePath, Tenant: cfg.Policy.Tenant, Site: cfg.Policy.Site, Device: cfg.Policy.Device, Clock: deps.Clock, Verifier: verifier, ValidatePin: m.validatePin}
		if m.remoteVerifier != nil {
			ac.RemoteVerifier, ac.ValidateRemoteAccepted = m.remoteVerifier, m.validateRemoteAccepted
		}
		if cfg.CreateProofArchive {
			m.archive, err = proofarchive.Create(ac)
		} else {
			m.archive, err = proofarchive.Open(ac)
		}
		if err != nil {
			return nil, err
		}
	}
	if err = m.openReplication(ctx); err != nil {
		return nil, err
	}
	m.broker, err = appservice.New(appservice.Config{Directory: cfg.Directory, UID: cfg.UID, GID: cfg.GID, Factory: m.publisher, PeerCheck: m.peer, Accept: m.accept, Query: m.query, PutEvidence: m.putEvidence, GetEvidence: m.getEvidence, Clock: m.readClock, PhysicalClock: m.readPhysicalClock, SourceAuthority: m.readSourceAuthority, GetRecordProof: m.getRecordProof, GetSensorFrame: m.readNativeCamera, FinalizeResponse: m.finalizeResponse, Limits: cfg.Limits, Trace: deps.Trace})
	if err != nil {
		return nil, err
	}
	ok = true
	return m, nil
}

func (m *Manager) Ensure(ctx context.Context, app, service string, generation uint64, publish []serviceorigin.PublishScope) (appservice.Mount, error) {
	return m.EnsureAccess(ctx, app, service, generation, publish, nil)
}

func (m *Manager) EnsureAccess(ctx context.Context, app, service string, generation uint64, publish []serviceorigin.PublishScope, subscribe []serviceaccess.Source) (appservice.Mount, error) {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	scope := m.cfg.Policy.scope(Grant{App: app, Service: service, Publish: publish}, generation)
	if m.policyCurrent(ctx) != nil || !m.permitsAccess(scope, subscribe) {
		return appservice.Mount{}, ErrDenied
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return appservice.Mount{}, ErrDenied
	}
	key := bindingKey(app, service)
	old := m.bindings[key]
	if old != nil && equalScopes(old.scope, scope) && equalSubscriptions(old.subscribe, subscribe) {
		m.mu.Unlock()
		mount, err := m.broker.Ensure(ctx, scope)
		if err == nil {
			m.mu.Lock()
			old.mount = mount
			m.mu.Unlock()
		}
		return mount, err
	}
	// Subscriptions are not part of the signed publication delegation. A read
	// entitlement change still closes the old listener and replaces its mount.
	m.mu.Unlock()
	if old != nil {
		m.broker.Release(app, service)
		if err := m.retireReplicaBinding(ctx, old); err != nil {
			return appservice.Mount{}, err
		}
	}
	m.mu.Lock()
	m.bindings[key] = &binding{scope: scope, subscribe: normalizedSubscriptions(subscribe)}
	m.mu.Unlock()
	mount, err := m.broker.Ensure(ctx, scope)
	if err != nil {
		m.mu.Lock()
		delete(m.bindings, key)
		m.mu.Unlock()
		return appservice.Mount{}, err
	}
	m.mu.Lock()
	m.bindings[key].mount = mount
	m.mu.Unlock()
	return mount, nil
}

func (m *Manager) ValidateMount(ctx context.Context, app, service string, generation uint64, directory string, publish []serviceorigin.PublishScope) error {
	return m.ValidateAccessMount(ctx, app, service, generation, directory, publish, nil)
}
func (m *Manager) ValidateAccessMount(ctx context.Context, app, service string, generation uint64, directory string, publish []serviceorigin.PublishScope, subscribe []serviceaccess.Source) error {
	if m.policyCurrent(ctx) != nil {
		return ErrDenied
	}
	scope := m.cfg.Policy.scope(Grant{App: app, Service: service, Publish: publish}, generation)
	m.mu.RLock()
	defer m.mu.RUnlock()
	b := m.bindings[bindingKey(app, service)]
	if m.closed || b == nil || b.mount.Directory != directory || !equalScopes(b.scope, scope) || !equalSubscriptions(b.subscribe, subscribe) || !m.deps.Identity.Current(b.token) {
		return ErrDenied
	}
	return nil
}

func (m *Manager) Release(app, service string, generation uint64) {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	m.mu.Lock()
	b := m.bindings[bindingKey(app, service)]
	if b == nil || b.scope.DeploymentGeneration != generation {
		m.mu.Unlock()
		return
	}
	if m.replica == nil {
		delete(m.bindings, bindingKey(app, service))
		m.mu.Unlock()
		m.broker.Release(app, service)
		return
	}
	m.mu.Unlock()
	m.broker.Release(app, service)
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	_ = m.retireReplicaBinding(ctx, b)
}
func (m *Manager) ReleaseApp(app string) {
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	if m.replica == nil {
		m.mu.Lock()
		for key, b := range m.bindings {
			if b.scope.App == app {
				delete(m.bindings, key)
			}
		}
		m.mu.Unlock()
		m.broker.ReleaseApp(app)
		return
	}
	m.mu.RLock()
	var bindings []*binding
	for _, b := range m.bindings {
		if b.scope.App == app {
			bindings = append(bindings, b)
		}
	}
	m.mu.RUnlock()
	m.broker.ReleaseApp(app)
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	for _, b := range bindings {
		_ = m.retireReplicaBinding(ctx, b)
	}
}
func (m *Manager) Close() error {
	if m == nil {
		return nil
	}
	m.lifecycle.Lock()
	defer m.lifecycle.Unlock()
	// Drain ordinary callbacks before taking any identity/remote fence. The
	// nil-replication shutdown order below remains the existing local path.
	if m.replica != nil && m.replica.started && !m.replica.failed.Load() {
		if m.broker != nil {
			_ = m.broker.Close()
		}
		m.mu.RLock()
		var bindings []*binding
		for _, b := range m.bindings {
			bindings = append(bindings, b)
		}
		m.mu.RUnlock()
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		for _, b := range bindings {
			_ = m.retireReplicaBinding(ctx, b)
		}
		cancel()
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	if m.broker != nil {
		_ = m.broker.Close()
	}
	var err error
	err = errors.Join(err, m.closeReplicaStores())
	if m.journal != nil {
		err = errors.Join(err, m.journal.Close())
	}
	if m.archive != nil {
		err = errors.Join(err, m.archive.Close())
	}
	if m.evidence != nil {
		err = errors.Join(err, m.evidence.Close())
	}
	if m.deps.SampleClose != nil {
		err = errors.Join(err, m.deps.SampleClose())
	}
	return err
}

func (m *Manager) publisher(ctx context.Context, scope serviceorigin.Scope) (appservice.Publisher, error) {
	if m.policyCurrent(ctx) != nil || !m.permits(scope) {
		return nil, ErrDenied
	}
	id, err := m.deps.Identity.Snapshot(ctx)
	if err != nil || id.Token == 0 || id.Invalidated == nil || !m.deps.Identity.Current(id.Token) {
		return nil, ErrDenied
	}
	w, err := m.physicalNow()
	if err != nil || w.Validate() != nil {
		return nil, ErrDenied
	}
	device, err := m.certificates.Verify(id.ChainDER, w.Earliest, w.Latest)
	if err != nil || m.identityAllowed(device) != nil {
		return nil, ErrDenied
	}
	epoch, err := m.journal.ReserveEpoch(ctx, scope.Tenant, scope.Device, scope.App, scope.Service)
	if err != nil {
		return nil, err
	}
	p, err := serviceorigin.NewPublisher(serviceorigin.PublisherConfig{Scope: scope, Epoch: epoch, Lifetime: time.Hour, Clock: m.deps.Clock, Certificates: m.certificates, ChainDER: id.ChainDER, DeviceKey: id.Key})
	if err != nil {
		return nil, err
	}
	from, until, err := p.AuthorityWindow()
	if err != nil {
		p.Close()
		return nil, err
	}
	public, err := p.PublicDelegation()
	var publicClaims serviceorigin.DelegationClaims
	if err != nil || jws.DecodeStrict(public.Claims, &publicClaims) != nil || !equalScopes(publicClaims.Scope, scope) || publicClaims.Epoch != epoch {
		p.Close()
		return nil, ErrDenied
	}
	publicBytes, err := json.Marshal(public)
	if err != nil {
		p.Close()
		return nil, ErrDenied
	}
	m.mu.Lock()
	b := m.bindings[bindingKey(scope.App, scope.Service)]
	if b == nil || !equalScopes(scope, b.scope) {
		m.mu.Unlock()
		p.Close()
		return nil, ErrDenied
	}
	b.token = id.Token
	b.epoch = epoch
	b.authorityFrom, b.authorityUntil = from, until
	b.delegation, b.delegationClaims, b.publisherDone, b.device = publicBytes, publicClaims, p.Done(), device
	m.mu.Unlock()
	g := &guardedPublisher{Publisher: p, manager: m, token: id.Token, invalidated: id.Invalidated}
	if g.Validate(ctx) != nil {
		p.Close()
		return nil, ErrDenied
	}
	if err := m.issueBindingLease(ctx, b); err != nil {
		p.Close()
		return nil, err
	}
	go func() {
		select {
		case <-m.ctx.Done():
		case <-id.Invalidated:
		case <-p.Done():
		}
		p.Close()
	}()
	return g, nil
}
func (m *Manager) peer(ctx context.Context, peer appservice.Peer, scope serviceorigin.Scope) error {
	if m.policyCurrent(ctx) != nil {
		return ErrDenied
	}
	m.mu.RLock()
	b := m.bindings[bindingKey(scope.App, scope.Service)]
	allowed := b != nil && equalScopes(b.scope, scope) && m.deps.Identity.Current(b.token)
	m.mu.RUnlock()
	if !allowed {
		return ErrDenied
	}
	return m.deps.PeerCheck(ctx, peer, scope)
}
func (m *Manager) accept(ctx context.Context, scope serviceorigin.Scope, record serviceorigin.SignedRecord, fence *appservice.Fence) error {
	m.mu.RLock()
	b := m.bindings[bindingKey(scope.App, scope.Service)]
	var token uint64
	if b != nil && equalScopes(b.scope, scope) {
		token = b.token
	}
	m.mu.RUnlock()
	if token == 0 {
		return ErrDenied
	}
	err := fence.Commit(ctx, func(ctx context.Context) error {
		return m.withCurrent(ctx, token, func() error {
			return m.identityOperation(ctx, token, func(ctx context.Context) error { return m.admitArchived(ctx, record) })
		})
	})
	if err == nil {
		// Offer only after complete successful admission and after all identity,
		// journal and replica fences have returned. This never performs I/O.
		m.offerReplicaLatest(record)
	}
	return err
}

// Snapshot is an in-agent diagnostic/read seam, not an app or admin RPC. Each
// returned original proof has passed current authorization and sample freshness.
func (m *Manager) Snapshot(ctx context.Context) ([]serviceorigin.SignedRecord, error) {
	if m.replica != nil && m.remoteOperation(ctx) == nil {
		var out []serviceorigin.SignedRecord
		err := m.withReplica(ctx, func(ctx context.Context) error {
			var e error
			out, e = m.Snapshot(ctx)
			return e
		})
		return out, err
	}
	proofs, err := m.journal.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return m.filterCurrent(ctx, proofs)
}

type guardedPublisher struct {
	*serviceorigin.Publisher
	manager     *Manager
	token       uint64
	invalidated <-chan struct{}
}

func (p *guardedPublisher) Validate(ctx context.Context) error {
	if ctx == nil {
		return ErrDenied
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-p.invalidated:
		p.Close()
		return ErrDenied
	default:
	}
	if !p.manager.deps.Identity.Current(p.token) || p.manager.policyCurrent(ctx) != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.Close()
		return ErrDenied
	}
	return p.Publisher.Validate(ctx)
}
func (p *guardedPublisher) Publish(ctx context.Context, request serviceorigin.PublishRequest) (serviceorigin.SignedRecord, error) {
	if err := p.Validate(ctx); err != nil {
		return serviceorigin.SignedRecord{}, err
	}
	r, err := p.Publisher.Publish(ctx, request)
	if err != nil {
		return serviceorigin.SignedRecord{}, err
	}
	if err = p.Validate(ctx); err != nil {
		return serviceorigin.SignedRecord{}, err
	}
	return r, nil
}
