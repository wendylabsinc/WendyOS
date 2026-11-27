package agentmodule

import (
	"bytes"
	"context"
	"crypto/x509"
	"log"
	"sync/atomic"

	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type replicaOperationKey struct{}
type replicaOperation struct {
	manager *Manager
	token   uint64
	remote  bool
	active  atomic.Bool
}

// A marker is an operation-scoped lock fact, not a credential or cached grant.
// It expires before its associated identity/remote fence is released.
func (m *Manager) identityOperation(ctx context.Context, token uint64, run func(context.Context) error) error {
	if m.replica == nil {
		return run(ctx)
	}
	op := &replicaOperation{manager: m, token: token}
	op.active.Store(true)
	defer op.active.Store(false)
	return run(context.WithValue(ctx, replicaOperationKey{}, op))
}

func (m *Manager) withReplica(ctx context.Context, run func(context.Context) error) error {
	r := m.replica
	if ctx == nil || r == nil || ctx.Err() != nil || r.failed.Load() {
		return ErrDenied
	}
	m.mu.RLock()
	token, started, closed := r.identity.Token, r.started, m.closed
	m.mu.RUnlock()
	if !started || closed || token == 0 || !m.deps.Identity.Current(token) {
		return ErrDenied
	}
	enter := func(ctx context.Context) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if ctx.Err() != nil || r.failed.Load() || m.policyCurrent(ctx) != nil || !m.deps.Identity.Current(token) {
			return ErrDenied
		}
		op := &replicaOperation{manager: m, token: token, remote: true}
		op.active.Store(true)
		defer op.active.Store(false)
		return run(context.WithValue(ctx, replicaOperationKey{}, op))
	}
	if value := ctx.Value(replicaOperationKey{}); value != nil {
		op, ok := value.(*replicaOperation)
		if !ok || op.manager != m || op.token != token || !op.active.Load() {
			return ErrDenied
		}
		if op.remote {
			return run(ctx)
		}
		return enter(ctx)
	}
	return m.withCurrent(ctx, token, func() error { return enter(ctx) })
}

func (m *Manager) replicaStoreGuard(ctx context.Context, run func() error) error {
	return m.withReplica(ctx, func(context.Context) error { return run() })
}

func (m *Manager) failReplication(stage string, cause error) {
	if r := m.replica; r != nil {
		first := r.failed.CompareAndSwap(false, true)
		r.cancel()
		if first {
			log.Printf("World replica stopped: stage=%s failure_class=%s", stage, replicaFailureClass(cause))
		}
	}
}

func cloneChain(in [][]byte) [][]byte {
	out := make([][]byte, len(in))
	for i, value := range in {
		out[i] = bytes.Clone(value)
	}
	return out
}

func (m *Manager) replicaIdentityAllowed(id wendycert.DeviceIdentity) error {
	if id.Tenant != m.cfg.Policy.Tenant {
		return ErrDenied
	}
	if id.Principal == m.cfg.Policy.Device {
		return m.identityAllowed(id)
	}
	peer := m.replica.peers[id.Principal]
	if peer == nil || peer.policy.CredentialSHA256 != id.CertificateSHA256 {
		return ErrDenied
	}
	for _, denied := range m.cfg.Policy.DeniedCertificates {
		for _, hash := range id.CandidateCertificateSHA256() {
			if hash == denied {
				return ErrDenied
			}
		}
	}
	for _, denied := range m.cfg.Policy.DeniedKeys {
		for _, hash := range id.CandidateSigningKeySHA256() {
			if hash == denied {
				return ErrDenied
			}
		}
	}
	return nil
}

// AuthenticateReplicaPeer accepts public certificates from the host's verified
// TLS state. It is not an RPC and cannot authenticate a request-supplied chain.
// The owned original chain is reverified at every later current-policy boundary.
func (m *Manager) AuthenticateReplicaPeer(ctx context.Context, chainDER [][]byte) (out replication.Identity, err error) {
	if len(chainDER) == 0 || len(chainDER) > 8 {
		return out, ErrDenied
	}
	total := 0
	for _, der := range chainDER {
		total += len(der)
		if len(der) == 0 || total > 64<<10 {
			return out, ErrDenied
		}
	}
	chain := cloneChain(chainDER)
	err = m.withReplica(ctx, func(ctx context.Context) error {
		w, e := m.physicalNow()
		if e != nil {
			return e
		}
		id, e := m.replicaCertificate(chain, w)
		if e != nil || id.Principal == m.cfg.Policy.Device || m.replicaIdentityAllowed(id) != nil {
			return ErrDenied
		}
		if m.policyCurrent(ctx) != nil {
			return ErrDenied
		}
		final, e := m.physicalNow()
		if e != nil || !m.policyWindowCurrent(final) || !final.Latest.Before(id.ValidUntil) || ctx.Err() != nil {
			return ErrDenied
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed || m.replica.failed.Load() || !m.deps.Identity.Current(m.replica.identity.Token) {
			return ErrDenied
		}
		m.replica.peers[id.Principal].chain = chain
		out = replication.Identity{Device: id.Principal, BindingSHA256: id.CertificateSHA256}
		return nil
	})
	if err != nil {
		return replication.Identity{}, err
	}
	return out, nil
}

func (m *Manager) replicaPeerWindow(ctx context.Context, identity replication.Identity, generation uint64, direction replication.Direction, sources []proofref.Source, w trustedtime.Interval) (servicejournal.AuthorityWindow, error) {
	r := m.replica
	if r == nil || ctx == nil || r.failed.Load() || ctx.Err() != nil || m.policyCurrent(ctx) != nil || w.Validate() != nil {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	peer := r.peers[identity.Device]
	if peer == nil || identity.BindingSHA256 != peer.policy.CredentialSHA256 || generation != peer.policy.Generation || len(sources) > 64 {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	allowed := peer.policy.Import
	if direction == replication.Export {
		allowed = peer.policy.Export
	} else if direction != replication.Import {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	if len(sources) == 0 && len(allowed) != 0 {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	seen := map[proofref.Source]bool{}
	for _, source := range sources {
		found := false
		for _, permit := range allowed {
			found = found || source == permit
		}
		if !found || seen[source] {
			return servicejournal.AuthorityWindow{}, ErrDenied
		}
		seen[source] = true
	}
	m.mu.RLock()
	chain := cloneChain(peer.chain)
	valid := !m.closed && r.started && m.deps.Identity.Current(r.identity.Token)
	m.mu.RUnlock()
	if !valid || len(chain) == 0 {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	id, e := m.replicaCertificate(chain, w)
	if e != nil || m.replicaIdentityAllowed(id) != nil || id.Principal != identity.Device {
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	until := earlier(m.cfg.Policy.NotAfter, id.ValidUntil)
	// Return immutable certificate/policy bounds, never the receipt time. The
	// journal intersects this window with an earlier sample in this operation.
	// Including every interval-eligible candidate can only narrow the grant.
	from := m.cfg.Policy.NotBefore
	for _, raw := range append(cloneChain(chain), m.cfg.Roots...) {
		cert, e := x509.ParseCertificate(raw)
		if e != nil {
			return servicejournal.AuthorityWindow{}, ErrDenied
		}
		if !w.Earliest.Before(cert.NotBefore) && w.Latest.Before(cert.NotAfter) && cert.NotBefore.After(from) {
			from = cert.NotBefore
		}
	}
	return servicejournal.AuthorityWindow{NotBefore: from, NotAfter: until}, nil
}
