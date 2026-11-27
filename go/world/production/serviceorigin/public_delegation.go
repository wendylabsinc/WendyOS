package serviceorigin

// PublicDelegation returns an owned copy of the publisher's original public
// certificate/delegation proof. It performs no signing, clock acquisition, key
// export or freshness assertion. Host readers must check the pinned publisher
// lifecycle, current identity/policy and original authority window at their
// response boundary. Closed/faulted publishers never return metadata.
func (p *Publisher) PublicDelegation() (SignedDelegation, error) {
	if p == nil || p.done == nil {
		return SignedDelegation{}, ErrClosed
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return SignedDelegation{}, p.err
	}
	return cloneDelegation(p.delegation), nil
}
