package serviceorigin

import "time"

// AuthorityWindow returns the immutable physical validity bounds certified for
// this publisher. The entire delegation window already fits one verified device
// certificate path. Hosts may pin these values when constructing a service
// binding and compare them against a later fresh authenticated interval.
//
// These values are metadata, not proof of current authority or a clock lease.
// Callers must still check revocation, exact binding, policy, current physical
// time and publisher invalidation. No clock read, key or signing method is
// exposed. Reading the metadata never renews the delegation.
func (p *Publisher) AuthorityWindow() (from, until time.Time, err error) {
	if p == nil || p.clock == nil {
		return time.Time{}, time.Time{}, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return time.Time{}, time.Time{}, p.err
	}
	return time.UnixMilli(p.claims.IssuedAtUnixMS), time.UnixMilli(p.claims.ExpiresAtUnixMS), nil
}
