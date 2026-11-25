package services

import (
	"context"
	"crypto/mldsa"
	"crypto/tls"
	"math"

	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
)

type worldIdentity struct{ provisioning *ProvisioningService }

// Called with provisioning.mu held before every live identity mutation. The
// token is atomic so journal authorization can recheck it inside WithCurrent.
func (s *ProvisioningService) invalidateWorldIdentityLocked() {
	old := s.worldIdentityToken.Load()
	if old == 0 || old == math.MaxUint64 {
		s.worldIdentityToken.Store(0)
	} else {
		s.worldIdentityToken.Store(old + 1)
	}
	if s.worldIdentityInvalidated != nil {
		close(s.worldIdentityInvalidated)
	}
	s.worldIdentityInvalidated = make(chan struct{})
}

func (i worldIdentity) Snapshot(ctx context.Context) (agentmodule.IdentitySnapshot, error) {
	if ctx == nil || ctx.Err() != nil {
		return agentmodule.IdentitySnapshot{}, agentmodule.ErrDenied
	}
	s := i.provisioning
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enrolled || s.resetCleanupErr != nil || s.resetRecoveryErr != nil || s.resetRestartPending || s.worldIdentityToken.Load() == 0 || s.worldIdentityInvalidated == nil {
		return agentmodule.IdentitySnapshot{}, agentmodule.ErrDenied
	}
	// Parse inside the agent. No copied key bytes or signing capability reach
	// the container socket, its mount, an RPC response, or a generic signer.
	pair, err := tls.X509KeyPair([]byte(s.certPEM+"\n"+s.chainPEM), s.keyPEM)
	if err != nil {
		return agentmodule.IdentitySnapshot{}, agentmodule.ErrDenied
	}
	key, ok := pair.PrivateKey.(*mldsa.PrivateKey)
	if !ok || key.PublicKey().Parameters() != mldsa.MLDSA65() {
		return agentmodule.IdentitySnapshot{}, agentmodule.ErrDenied
	}
	return agentmodule.IdentitySnapshot{Token: s.worldIdentityToken.Load(), ChainDER: pair.Certificate, Key: key, Invalidated: s.worldIdentityInvalidated}, nil
}
func (i worldIdentity) Current(token uint64) bool {
	return token != 0 && i.provisioning.worldIdentityToken.Load() == token
}
func (i worldIdentity) WithCurrent(token uint64, fn func() error) error {
	s := i.provisioning
	// Keep all existing data callbacks serialized. Clock and immutable public
	// authority reads may bypass this gate through WithCurrentClock. Acquire
	// the work gate before the identity read lock, so queued data work cannot hold readers
	// open while a provisioning writer waits to invalidate the identity.
	s.worldOperationMu.Lock()
	defer s.worldOperationMu.Unlock()
	return i.withCurrentRead(token, fn)
}

// WithCurrentClock pins the same identity as WithCurrent throughout the
// callback, but allows clock RPCs to overlap existing data work. Callbacks
// must never reenter either method, Snapshot, or a provisioning API: an
// exclusive writer may be waiting and RWMutex does not allow recursive reads.
func (i worldIdentity) WithCurrentClock(token uint64, fn func() error) error {
	return i.withCurrentRead(token, fn)
}

func (i worldIdentity) withCurrentRead(token uint64, fn func() error) error {
	s := i.provisioning
	s.mu.RLock()
	defer s.mu.RUnlock()
	if fn == nil || !s.enrolled || s.resetCleanupErr != nil || s.resetRecoveryErr != nil || s.resetRestartPending || !i.Current(token) {
		return agentmodule.ErrDenied
	}
	return fn()
}
