package meshalpn

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
)

const ticketIssuers = "wendy-known-ca/1\x00"

// Tickets retain only fingerprints of omitted path candidates. If a later
// handshake no longer has one, decline resumption and exchange certificates
// normally. This also works when a client ticket survives a process restart.
type handshakeIssuers struct {
	available    []*x509.Certificate
	dependencies []Fingerprint
}

func (h *handshakeIssuers) accepts(state *tls.SessionState) bool {
	for _, entry := range state.Extra {
		if !bytes.HasPrefix(entry, []byte(ticketIssuers)) {
			continue
		}
		data := entry[len(ticketIssuers):]
		if len(data)%FingerprintSize != 0 || len(data) > 32*FingerprintSize {
			return false
		}
		for len(data) > 0 {
			var want Fingerprint
			copy(want[:], data[:FingerprintSize])
			data = data[FingerprintSize:]
			found := false
			for _, cert := range h.available {
				if Hash(cert.Raw) == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}
func (h *handshakeIssuers) stamp(state *tls.SessionState) *tls.SessionState {
	result := *state
	result.Extra = append([][]byte(nil), state.Extra...)
	// An inner first-contact collector may have been configured after the
	// handshake. Preserve the dependencies already supplied by its outer wrapper.
	if len(h.dependencies) == 0 {
		return &result
	}
	result.Extra = nil
	for _, entry := range state.Extra {
		if !bytes.HasPrefix(entry, []byte(ticketIssuers)) {
			result.Extra = append(result.Extra, entry)
		}
	}
	entry := make([]byte, len(ticketIssuers), len(ticketIssuers)+len(h.dependencies)*FingerprintSize)
	copy(entry, ticketIssuers)
	for _, hash := range h.dependencies {
		entry = append(entry, hash[:]...)
	}
	result.Extra = append(result.Extra, entry)
	return &result
}
func (h *handshakeIssuers) configureTickets(config *tls.Config) {
	if config.ClientSessionCache != nil {
		config.ClientSessionCache = &issuerSessionCache{next: config.ClientSessionCache, loan: h}
	}
	wrap, unwrap := config.WrapSession, config.UnwrapSession
	if wrap != nil {
		config.WrapSession = func(cs tls.ConnectionState, state *tls.SessionState) ([]byte, error) { return wrap(cs, h.stamp(state)) }
	}
	if unwrap != nil {
		config.UnwrapSession = func(ticket []byte, cs tls.ConnectionState) (*tls.SessionState, error) {
			state, err := unwrap(ticket, cs)
			if err == nil && state != nil && !h.accepts(state) {
				return nil, nil
			}
			return state, err
		}
	}
}

type issuerSessionCache struct {
	next tls.ClientSessionCache
	loan *handshakeIssuers
}

// Underlying preserves access to a first-contact collector through this wrapper.
func (c *issuerSessionCache) Underlying() tls.ClientSessionCache { return c.next }
func (c *issuerSessionCache) Get(key string) (*tls.ClientSessionState, bool) {
	session, ok := c.next.Get(key)
	if !ok || session == nil {
		return nil, false
	}
	_, state, err := session.ResumptionState()
	if err != nil || state == nil || !c.loan.accepts(state) {
		return nil, false
	}
	return session, true
}
func (c *issuerSessionCache) Put(key string, session *tls.ClientSessionState) {
	if session == nil {
		c.next.Put(key, nil)
		return
	}
	ticket, state, err := session.ResumptionState()
	if err != nil || state == nil {
		return
	}
	next, err := tls.NewResumptionState(ticket, c.loan.stamp(state))
	if err == nil {
		c.next.Put(key, next)
	}
}
