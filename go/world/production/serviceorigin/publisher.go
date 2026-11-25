package serviceorigin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"crypto/rand"
	"math"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

// Publisher holds its short-lived Ed25519 key inside the agent. Only structured
// Publish is exposed; neither key export nor arbitrary signing is available.
type Publisher struct {
	mu           sync.Mutex
	clock        trustedtime.Provider
	certificates *wendycert.DeviceVerifier
	claims       DelegationClaims
	delegation   SignedDelegation
	key          ed25519.PrivateKey
	sequence     uint64
	last         trustedtime.Interval
	done         chan struct{}
	err          error
	timer        *time.Timer
}

func NewPublisher(cfg PublisherConfig) (*Publisher, error) {
	if validateScope(cfg.Scope) != nil || cfg.Epoch == 0 || cfg.Lifetime < time.Millisecond || cfg.Lifetime > MaxLifetime || cfg.Clock == nil || cfg.Certificates == nil || !chainBounds(cfg.ChainDER) || cfg.DeviceKey == nil || cfg.DeviceKey.PublicKey().Parameters() != mldsa.MLDSA65() {
		return nil, ErrDenied
	}
	before, err := cfg.Clock.Now()
	if err != nil || before.Validate() != nil {
		return nil, ErrClock
	}
	chain := cloneDelegation(SignedDelegation{ChainDER: cfg.ChainDER}).ChainDER
	id, err := cfg.Certificates.Verify(chain, before.Earliest, before.Latest)
	if err != nil || id.Principal != cfg.Scope.Device || id.Tenant != cfg.Scope.Tenant {
		return nil, ErrDenied
	}
	issued := before.Earliest.UnixMilli()
	expires := before.Earliest.Add(cfg.Lifetime).UnixMilli()
	if id.ValidUntil.UnixMilli() < expires {
		expires = id.ValidUntil.UnixMilli()
	}
	if !validWindow(issued, expires) || !within(before, issued, expires) {
		return nil, ErrExpired
	}
	// The complete delegation window, not just the present, must fit one
	// verified certificate path. No certificate NotBefore is inferred.
	if _, err = cfg.Certificates.Verify(chain, time.UnixMilli(issued), time.UnixMilli(expires)); err != nil {
		return nil, ErrDenied
	}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	owned := false
	defer func() {
		if !owned {
			clear(key)
		}
	}()
	claims := DelegationClaims{Version: Version, AuthorityDomain: AuthorityDomain, Scope: cloneScope(cfg.Scope), Epoch: cfg.Epoch, IssuedAtUnixMS: issued, ExpiresAtUnixMS: expires, PublicKey: public}
	raw, err := marshalBounded(claims, MaxClaimsBytes)
	if err != nil {
		return nil, err
	}
	signature, err := cfg.DeviceKey.Sign(nil, append([]byte(delegationPrefix), raw...), &mldsa.Options{})
	if err != nil || len(signature) != mldsa.MLDSA65SignatureSize || id.VerifySignature(append([]byte(delegationPrefix), raw...), signature) != nil {
		return nil, ErrDenied
	}
	after, err := cfg.Clock.Now()
	if err != nil || !forward(before, after) {
		return nil, ErrClock
	}
	if !within(after, issued, expires) {
		return nil, ErrExpired
	}
	if _, err = cfg.Certificates.Verify(chain, after.Earliest, after.Latest); err != nil {
		return nil, ErrDenied
	}
	p := &Publisher{clock: cfg.Clock, certificates: cfg.Certificates, claims: claims, delegation: SignedDelegation{Claims: raw, ChainDER: chain, Signature: signature}, key: key, last: after, done: make(chan struct{})}
	// The timer is an additional conservative expiry fence. Validate must also
	// run on calls/idle polls because the trusted provider includes suspension
	// and clock faults which an ordinary timer cannot establish on its own.
	p.mu.Lock()
	p.timer = time.AfterFunc(time.UnixMilli(expires).Sub(after.Latest), func() { p.mu.Lock(); defer p.mu.Unlock(); p.invalidateLocked(ErrExpired) })
	p.mu.Unlock()
	owned = true
	return p, nil
}

func (p *Publisher) invalidateLocked(err error) {
	if p.err != nil {
		return
	}
	p.err = err
	clear(p.key)
	p.key = nil
	if p.timer != nil {
		p.timer.Stop()
	}
	close(p.done)
}
func (p *Publisher) validateLocked(ctx context.Context) (trustedtime.Interval, error) {
	if err := ctx.Err(); err != nil {
		return trustedtime.Interval{}, err
	}
	if p.err != nil {
		return trustedtime.Interval{}, p.err
	}
	w, err := p.clock.Now()
	if err != nil || !forward(p.last, w) {
		p.invalidateLocked(ErrClock)
		return trustedtime.Interval{}, ErrClock
	}
	if !within(w, p.claims.IssuedAtUnixMS, p.claims.ExpiresAtUnixMS) {
		p.invalidateLocked(ErrExpired)
		return trustedtime.Interval{}, ErrExpired
	}
	if _, err = p.certificates.Verify(p.delegation.ChainDER, w.Earliest, w.Latest); err != nil {
		p.invalidateLocked(ErrDenied)
		return trustedtime.Interval{}, ErrDenied
	}
	if err = ctx.Err(); err != nil {
		return trustedtime.Interval{}, err
	}
	p.last = w
	return w, nil
}

func (p *Publisher) Validate(ctx context.Context) error {
	if p == nil || p.clock == nil || ctx == nil {
		return ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.validateLocked(ctx)
	return err
}
func (p *Publisher) Publish(ctx context.Context, request PublishRequest) (SignedRecord, error) {
	if p == nil || p.clock == nil || ctx == nil {
		return SignedRecord{}, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	before, err := p.validateLocked(ctx)
	if err != nil {
		return SignedRecord{}, err
	}
	request = cloneRequest(request)
	if validateRequest(request, p.claims.Scope) != nil {
		return SignedRecord{}, ErrDenied
	}
	if p.sequence == math.MaxUint64 {
		p.invalidateLocked(ErrDenied)
		return SignedRecord{}, ErrDenied
	}
	// Consume before signing. Cancellation or a final clock failure cannot
	// cause a sequence to be reused for a different record.
	p.sequence++
	record := Record{Version: Version, AuthorityDomain: AuthorityDomain, DelegationSHA256: hash(p.delegation.Claims), Scope: cloneScope(p.claims.Scope), Epoch: p.claims.Epoch, Sequence: p.sequence, IssuedAtUnixMS: before.Earliest.UnixMilli(), ExpiresAtUnixMS: p.claims.ExpiresAtUnixMS, Publication: request}
	raw, err := marshalBounded(record, MaxRecordBytes)
	if err != nil {
		return SignedRecord{}, err
	}
	signature := ed25519.Sign(p.key, append([]byte(recordPrefix), raw...))
	after, err := p.validateLocked(ctx)
	if err != nil {
		return SignedRecord{}, err
	}
	if !forward(before, after) {
		p.invalidateLocked(ErrClock)
		return SignedRecord{}, ErrClock
	}
	return SignedRecord{Delegation: cloneDelegation(p.delegation), Record: bytes.Clone(raw), Signature: signature}, nil
}
func (p *Publisher) Close() error {
	if p == nil || p.done == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.invalidateLocked(ErrClosed)
	return nil
}
func (p *Publisher) Done() <-chan struct{} {
	if p == nil || p.done == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return p.done
}
func (p *Publisher) Err() error {
	if p == nil || p.clock == nil {
		return ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}
