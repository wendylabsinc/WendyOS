// Package authorizer composes Wendy request verification, authenticated time,
// explicit site policy and durable replay admission. It exposes no HTTP route.
package authorizer

import (
	"errors"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/admission"
	"github.com/wendylabsinc/wendy/go/world/production/request"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

type Config struct {
	Admission admission.Config
	Clock     trustedtime.Provider
}

type Authorizer struct {
	mu        sync.Mutex
	cfg       Config
	store     *admission.Store
	closed    bool
	lastTime  *trustedtime.Interval
	timeFault bool
}

// Decision records one completed read admission, not a reusable capability.
// It cannot authorize publication, a mission, hardware, or another request.
type Decision struct {
	Principal, Tenant, Site, Audience, Operation string
	PolicyRevision, RevocationRevision           uint64
	CheckedAt                                    trustedtime.Interval
}

func Open(path string, cfg Config) (*Authorizer, error) {
	if cfg.Clock == nil || cfg.Admission.Certificates == nil {
		return nil, errors.New("explicit certificate trust and authenticated clock required")
	}
	store, err := admission.Open(path, cfg.Admission)
	if err != nil {
		return nil, err
	}
	return &Authorizer{cfg: cfg, store: store}, nil
}

func (a *Authorizer) window() (trustedtime.Interval, error) {
	if a.timeFault {
		return trustedtime.Interval{}, trustedtime.ErrUntrusted
	}
	w, err := a.cfg.Clock.Now()
	if err != nil || w.Validate() != nil {
		return trustedtime.Interval{}, trustedtime.ErrUntrusted
	}
	if a.lastTime != nil && continuous(*a.lastTime, w) != nil {
		a.timeFault = true
		return trustedtime.Interval{}, trustedtime.ErrUntrusted
	}
	a.lastTime = &w
	return w, nil
}

func (a *Authorizer) ApplyPolicy(compact string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return admission.ErrClosed
	}
	w, err := a.window()
	if err != nil {
		return err
	}
	return a.store.ApplyPolicy(compact, w)
}

func (a *Authorizer) ApplyRevocations(compact string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return admission.ErrClosed
	}
	w, err := a.window()
	if err != nil {
		return err
	}
	return a.store.ApplyRevocations(compact, w)
}

// Snapshot is local diagnostic state. Its policy/revocation versions confer no
// permission, and reading it neither verifies nor consumes a request proof.
func (a *Authorizer) Snapshot() (admission.Snapshot, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return admission.Snapshot{}, admission.ErrClosed
	}
	return a.store.Snapshot()
}

// Authorize accepts raw bytes, never a caller-supplied "verified" identity.
// Policy updates and admissions share one lock. A nonce stays consumed if the
// final time or authority check fails after its durable commit.
func (a *Authorizer) Authorize(compact, method, path string, body []byte) (Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var denied Decision
	if a.closed {
		return denied, admission.ErrClosed
	}
	if method != request.Method || path != request.Path || len(body) > request.MaxBodyBytes {
		return denied, admission.ErrDenied
	}
	body = append([]byte(nil), body...)
	w, err := a.window()
	if err != nil {
		return denied, err
	}
	snapshot, err := a.store.Snapshot()
	if err != nil {
		return denied, err
	}
	cfg := a.cfg.Admission
	expected := request.Expected{Tenant: cfg.Tenant, Site: cfg.Site, Audience: cfg.Audience, Operation: request.OperationRead, Method: method, Path: path, PolicyRevision: snapshot.PolicyRevision}
	proof, err := request.Verify(compact, expected, body, cfg.Certificates, w.Earliest, w.Latest)
	if err != nil {
		return denied, err
	}
	next, err := a.window()
	if err != nil || continuous(w, next) != nil || validProof(proof, next) != nil {
		return denied, admission.ErrDenied
	}
	d := proof.Descriptor
	r := admission.Request{Identity: proof.Identity, Tenant: d.Tenant, Site: d.Site, Audience: d.Audience, Principal: d.Principal, Operation: d.Operation, PolicyRevision: d.PolicyRevision, Nonce: d.Nonce, IssuedAtUnixMS: d.IssuedAtUnixMS, ExpiresAtUnixMS: d.ExpiresAtUnixMS}
	if err = a.store.Admit(r, next); err != nil {
		return denied, err
	}
	final, err := a.window()
	if err != nil || continuous(next, final) != nil || validProof(proof, final) != nil {
		return denied, admission.ErrDenied
	}
	// This is deliberately after the durable nonce write. Recheck authority
	// at the final sampled interval, including time spent in verification/fsync.
	if err = a.store.Check(r, final); err != nil {
		return denied, err
	}
	return Decision{Principal: d.Principal, Tenant: d.Tenant, Site: d.Site, Audience: d.Audience, Operation: d.Operation, PolicyRevision: snapshot.PolicyRevision, RevocationRevision: snapshot.RevocationRevision, CheckedAt: final}, nil
}

func continuous(before, after trustedtime.Interval) error {
	if after.Validate() != nil || before.BootID != after.BootID || after.Monotonic < before.Monotonic || after.Latest.Before(before.Earliest) {
		return trustedtime.ErrUntrusted
	}
	return nil
}

func validProof(p request.Proof, w trustedtime.Interval) error {
	if w.Earliest.Before(p.Identity.ValidFrom) || !w.Latest.Before(p.Identity.ValidUntil) || w.Earliest.Before(time.UnixMilli(p.Descriptor.IssuedAtUnixMS)) || !w.Latest.Before(time.UnixMilli(p.Descriptor.ExpiresAtUnixMS)) {
		return admission.ErrDenied
	}
	return nil
}

func (a *Authorizer) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	return a.store.Close()
}
