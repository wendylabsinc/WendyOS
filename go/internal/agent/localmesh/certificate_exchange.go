package localmesh

import (
	"errors"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// CertificateChain describes exact bytes in leaf-to-issuer order. It is a
// fetch hint, never a trust assertion. The full device principal stays the
// identity-cache key after independent verification of the assembled chain.
type CertificateChain struct {
	Principal string   `json:"principal" cbor:"p"`
	Hashes    []string `json:"hashes" cbor:"h"`
}

type CertificateRequest struct {
	Hash   string `json:"hash" cbor:"h"`
	Offset int    `json:"offset" cbor:"f"`
}

type CertificateChunk struct {
	Hash   string `json:"hash" cbor:"h"`
	Offset int    `json:"offset" cbor:"f"`
	Total  int    `json:"total" cbor:"l"`
	Data   []byte `json:"data" cbor:"d"`
}

type CertificateMessage struct {
	Chain   *CertificateChain   `json:"chain,omitempty" cbor:"c,omitempty"`
	Request *CertificateRequest `json:"request,omitempty" cbor:"r,omitempty"`
	Chunk   *CertificateChunk   `json:"chunk,omitempty" cbor:"d,omitempty"`
	// Missing retires only this supplier for the exact requested hash/offset.
	Missing *CertificateRequest `json:"missing,omitempty" cbor:"m,omitempty"`
}

// Validate checks the bounded transfer envelope without granting trust.
func (m CertificateMessage) Validate() error { return m.validate() }

func (m CertificateMessage) validate() error {
	fields := 0
	if c := m.Chain; c != nil {
		fields++
		if len(c.Principal) > maxPrincipalBytes || len(c.Hashes) == 0 || len(c.Hashes) > 16 {
			return errors.New("invalid certificate chain description")
		}
		if _, err := meshid.PeerFromPrincipal(c.Principal); err != nil {
			return err
		}
		seen := make(map[string]bool)
		for _, hash := range c.Hashes {
			if !validCertificateHash(hash) || seen[hash] {
				return errors.New("invalid or repeated certificate hash")
			}
			seen[hash] = true
		}
	}
	for _, q := range []*CertificateRequest{m.Request, m.Missing} {
		if q != nil {
			fields++
			if !validCertificateHash(q.Hash) || q.Offset < 0 || q.Offset >= maxCertificateBytes || q.Offset%certificateChunkBytes != 0 {
				return errors.New("invalid certificate request")
			}
		}
	}
	if c := m.Chunk; c != nil {
		fields++
		if !validCertificateHash(c.Hash) || c.Total <= 0 || c.Total > maxCertificateBytes || c.Offset < 0 || c.Offset%certificateChunkBytes != 0 || len(c.Data) != min(certificateChunkBytes, c.Total-c.Offset) || len(c.Data) == 0 {
			return errors.New("invalid certificate chunk")
		}
	}
	// An unknown optional certificate control decodes to zero known fields.
	// Consume it without interpreting it as any known operation.
	if fields > 1 {
		return errors.New("ambiguous certificate message")
	}
	return nil
}

type pendingCertificateChain struct {
	chain   CertificateChain
	expires time.Time
}

// CertificateExchange is owned by one authenticated neighbour incarnation.
// Poll/Receive run on its protocol actor; Close releases abandoned claims.
// The pool is shared across neighbours, so only the first supplier is asked
// for a missing element. Each exchange has at most one chunk request in flight.
type CertificateExchange struct {
	cache   *IdentityCache
	tenant  meshid.Tenant
	owner   uint64
	pending map[string]pendingCertificateChain
	waiting *CertificateRequest
	closed  bool
}

func NewCertificateExchange(cache *IdentityCache, tenant meshid.Tenant) *CertificateExchange {
	return &CertificateExchange{cache: cache, tenant: tenant, owner: cache.parts.newOwner(), pending: make(map[string]pendingCertificateChain)}
}

func (e *CertificateExchange) Close() {
	e.closed = true
	clear(e.pending)
	e.waiting = nil
	e.cache.parts.release(e.owner)
}

// forgetPrincipal cancels only this supplier's exact descriptor. A late
// identity-missing can follow a descriptor/partial body already in flight.
// Release its partial claim now so another supplier need not await timeout.
func (e *CertificateExchange) forgetPrincipal(principal string, now time.Time) {
	for key, p := range e.pending {
		if p.chain.Principal == principal {
			e.forgetChain(key, now)
		}
	}
}
func (e *CertificateExchange) forgetChain(principal string, now time.Time) {
	pending, ok := e.pending[principal]
	if !ok {
		return
	}
	delete(e.pending, principal)
	if e.waiting == nil {
		return
	}
	needed := false
	for _, hash := range pending.chain.Hashes {
		if hash == e.waiting.Hash {
			needed = true
			break
		}
	}
	if !needed {
		return
	}
	for _, other := range e.pending {
		for _, hash := range other.chain.Hashes {
			if hash == e.waiting.Hash {
				return
			}
		}
	}
	e.cache.parts.unavailable(e.waiting.Hash, e.owner, now)
	e.waiting = nil
}

// Pending reports bounded chain assembly owned by this protocol actor.
func (e *CertificateExchange) Pending() bool { return len(e.pending) != 0 }

// PendingChain returns an owned exact descriptor and its absolute fetch
// deadline only while this neighbour's exchange is actively fetching it.
// It grants no trust; any retained payload must still pass normal verification
// after completion. A caller retaining a payload must keep its original
// deadline even if repeated descriptions later refresh the exchange's hint.
func (e *CertificateExchange) PendingChain(principal string, now time.Time) (CertificateChain, time.Time, bool) {
	p, ok := e.pending[principal]
	if !ok {
		for _, candidate := range e.pending {
			if candidate.chain.Principal == principal {
				p, ok = candidate, true
				break
			}
		}
	}
	if e.closed || !ok || !p.expires.After(now) {
		return CertificateChain{}, time.Time{}, false
	}
	p.chain.Hashes = append([]string(nil), p.chain.Hashes...)
	return p.chain, p.expires, true
}

func sameCertificateDescription(a, b CertificateChain) bool {
	if a.Principal != b.Principal || len(a.Hashes) != len(b.Hashes) {
		return false
	}
	for i := range a.Hashes {
		if a.Hashes[i] != b.Hashes[i] {
			return false
		}
	}
	return true
}

func matchesCertificateDescription(chain [][]byte, d CertificateChain) bool {
	if len(chain) != len(d.Hashes) {
		return false
	}
	for i := range chain {
		if certificateHash(chain[i]) != d.Hashes[i] {
			return false
		}
	}
	return true
}

func (e *CertificateExchange) Describe(principal string, now time.Time) *CertificateMessage {
	if e.closed {
		return nil
	}
	chain, _, ok := e.cache.Get(principal, now)
	if !ok {
		return nil
	}
	return DescribeCertificates(principal, chain)
}

// DescribeCertificates names exact chain bytes; it grants no identity or trust.
func DescribeCertificates(principal string, chain [][]byte) *CertificateMessage {
	c := &CertificateChain{Principal: principal}
	for _, data := range chain {
		if len(data) == 0 || len(data) > maxCertificateBytes {
			return nil
		}
		c.Hashes = append(c.Hashes, certificateHash(data))
	}
	return &CertificateMessage{Chain: c}
}

// Receive returns only a bounded descriptor/request/chunk response. Completed
// principals have already passed this cache's unchanged full-chain verifier.
func (e *CertificateExchange) Receive(m CertificateMessage, now time.Time, slots ...int) (replies []*CertificateMessage, completed []string, err error) {
	if len(slots) > 0 && slots[0] < 1 {
		return nil, nil, ErrControlBackpressure
	}
	if e.closed {
		return nil, nil, errors.New("certificate exchange ended")
	}
	if err := m.validate(); err != nil {
		return nil, nil, err
	}
	if c := m.Chain; c != nil {
		device, _ := meshid.PeerFromPrincipal(c.Principal)
		if device.Tenant() != e.tenant {
			return nil, nil, errors.New("foreign certificate chain description")
		}
		key := c.Principal
		if device.IsOperator() {
			key += "#" + c.Hashes[0]
		}
		if _, found := e.pending[key]; found || len(e.pending) < 128 {
			e.pending[key] = pendingCertificateChain{chain: CertificateChain{Principal: c.Principal, Hashes: append([]string(nil), c.Hashes...)}, expires: now.Add(MaxLease)}
		}
	}
	if q := m.Request; q != nil {
		data := e.cache.certificate(q.Hash, now)
		if q.Offset < len(data) {
			end := min(len(data), q.Offset+certificateChunkBytes)
			replies = append(replies, &CertificateMessage{Chunk: &CertificateChunk{Hash: q.Hash, Offset: q.Offset, Total: len(data), Data: append([]byte(nil), data[q.Offset:end]...)}})
		} else {
			copy := *q
			replies = append(replies, &CertificateMessage{Missing: &copy})
		}
	}
	if m.Missing != nil && e.waiting != nil && *m.Missing == *e.waiting {
		missing := e.MissingPrincipals(m.Missing)
		e.cache.parts.unavailable(m.Missing.Hash, e.owner, now)
		e.waiting = nil
		for _, principal := range missing {
			e.forgetPrincipal(principal, now)
		}
	}
	if c := m.Chunk; c != nil && e.waiting != nil && e.waiting.Hash == c.Hash {
		if c.Offset != e.waiting.Offset {
			return nil, nil, errors.New("unordered certificate chunk")
		}
		next, accepted, err := e.cache.parts.accept(e.owner, *c, now)
		if err != nil {
			return nil, nil, err
		}
		e.waiting = nil
		if accepted && next != 0 {
			e.waiting = &CertificateRequest{Hash: c.Hash, Offset: next}
			replies = append(replies, &CertificateMessage{Request: e.waiting})
		}
	}
	budget := maxCertificatePending + 2
	if len(slots) > 0 {
		budget = max(0, slots[0]-len(replies))
	}
	more, completed, err := e.Poll(now, budget)
	return append(replies, more...), completed, err
}

// Poll also completes chains whose missing element was supplied by another
// neighbour. Call on the existing maintenance tick and after received data.
func (e *CertificateExchange) Poll(now time.Time, slots ...int) (replies []*CertificateMessage, completed []string, err error) {
	if e.closed {
		return nil, nil, nil
	}
	// A completed principal can make its retained manifest emit one identity
	// repair response. Charge a conservative slot before verification/removal,
	// not after the caller has already lost the pending continuation.
	budget := maxCertificatePending + 2
	if len(slots) > 0 {
		budget = max(0, slots[0])
	}
	if e.waiting != nil && !e.cache.parts.owned(e.waiting.Hash, e.owner, now) {
		e.waiting = nil
	}
	principals := make([]string, 0, len(e.pending))
	for principal := range e.pending {
		principals = append(principals, principal)
	}
	sort.Strings(principals)
	for _, principal := range principals {
		pending := e.pending[principal]
		if !pending.expires.After(now) {
			delete(e.pending, principal)
			continue
		}
		if budget == 0 {
			continue
		}
		chain := make([][]byte, 0, len(pending.chain.Hashes))
		for _, hash := range pending.chain.Hashes {
			data := e.cache.certificate(hash, now)
			if data == nil {
				if budget > 0 && e.waiting == nil && e.cache.parts.claim(hash, e.owner, now) {
					e.waiting = &CertificateRequest{Hash: hash}
					replies = append(replies, &CertificateMessage{Request: e.waiting})
					budget--
				}
				break // Learn one missing chain element before requesting another.
			}
			chain = append(chain, data)
		}
		if len(chain) != len(pending.chain.Hashes) {
			continue
		}
		_, err := e.cache.put(chain, pending.chain.Principal, now)
		if err != nil {
			return nil, nil, err
		}
		delete(e.pending, principal)
		completed = append(completed, pending.chain.Principal)
		budget--
	}
	return replies, completed, nil
}

func (p *CertificatePool) owned(hash string, owner uint64, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	return p.pending[hash] != nil && p.pending[hash].owner == owner
}

func (p *CertificatePool) accept(owner uint64, chunk CertificateChunk, now time.Time) (next int, accepted bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.prune(now)
	fetch := p.pending[chunk.Hash]
	if fetch == nil || fetch.owner != owner {
		return 0, false, nil // No retained state for unsolicited or old-session data.
	}
	if fetch.total == 0 {
		if !p.room(chunk.Total, false) {
			return 0, false, errors.New("certificate assembly capacity reached")
		}
		fetch.total = chunk.Total
		fetch.data = make([]byte, 0, chunk.Total)
		p.bytes += chunk.Total
	}
	if fetch.total != chunk.Total || chunk.Offset != len(fetch.data) {
		return 0, false, errors.New("unordered certificate assembly")
	}
	fetch.data = append(fetch.data, chunk.Data...)
	fetch.expires = now.Add(certificateFetchTTL)
	if len(fetch.data) < fetch.total {
		return len(fetch.data), true, nil
	}
	delete(p.pending, chunk.Hash)
	p.bytes -= fetch.total
	if certificateHash(fetch.data) != chunk.Hash {
		return 0, false, errors.New("certificate fingerprint mismatch")
	}
	if !p.room(fetch.total, true) {
		return 0, false, errors.New("certificate object capacity reached")
	}
	p.objects[chunk.Hash] = certificateObject{data: fetch.data, used: now, expires: now.Add(MaxLease)}
	p.bytes += fetch.total
	return 0, true, nil
}

// ErrCertificateNeeded means an advertised element is absent from both the
// shared public-byte pool and this cache's current verified identities.
var ErrCertificateNeeded = errors.New("certificate element needed")

// ResolveCertificateChain admits a description only after finding all its
// exact bytes and applying the unchanged principal-scoped chain verifier.
func (c *IdentityCache) ResolveCertificateChain(description CertificateChain, now time.Time) (string, error) {
	if err := (CertificateMessage{Chain: &description}).Validate(); err != nil {
		return "", err
	}
	chain := make([][]byte, 0, len(description.Hashes))
	for _, hash := range description.Hashes {
		data := c.certificate(hash, now)
		if data == nil {
			return "", ErrCertificateNeeded
		}
		chain = append(chain, data)
	}
	return c.put(chain, description.Principal, now)
}

// MissingPrincipals identifies only chains blocked by the outstanding request.
// It allows directory adapters to discard their matching unverified artifacts,
// releasing shared gossip claims so another neighbour can supply them now.
func (e *CertificateExchange) MissingPrincipals(q *CertificateRequest) []string {
	if q == nil || e.waiting == nil || *q != *e.waiting {
		return nil
	}
	var principals []string
	for _, p := range e.pending {
		for _, hash := range p.chain.Hashes {
			if hash == q.Hash {
				principals = append(principals, p.chain.Principal)
				break
			}
		}
	}
	return principals
}
