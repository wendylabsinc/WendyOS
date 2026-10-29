package meshalpn

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"strings"
)

const ticketIssuers = "wendy-known-ca/1\x00"
const ticketProtocol = "wendy-known-ca-alpn/1\x00"

// Tickets retain only fingerprints of omitted path candidates. If a later
// handshake no longer has one, decline resumption and exchange certificates
// normally. This also works when a client ticket survives a process restart.
type handshakeIssuers struct {
	available    []*x509.Certificate
	dependencies []Fingerprint
	protocol     string
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
	if h.protocol != "" {
		kept := result.Extra[:0]
		for _, entry := range result.Extra {
			if !bytes.HasPrefix(entry, []byte(ticketProtocol)) {
				kept = append(kept, entry)
			}
		}
		result.Extra = append(kept, []byte(ticketProtocol+h.protocol))
	}
	// An inner first-contact collector may have been configured after the
	// handshake. Preserve the dependencies already supplied by its outer wrapper.
	if len(h.dependencies) == 0 {
		return &result
	}
	entries := result.Extra
	result.Extra = nil
	for _, entry := range entries {
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

// Keep a reusable ticket's negotiated alias stable as the CA cache warms.
// Offering a newly available longer prefix would make TLS reject that ticket
// solely because its ALPN changed. Missing dependencies or a changed local path
// instead use the normal full set of offers and a fresh handshake.
func resumptionProtocols(config *tls.Config, offered []string, available []*x509.Certificate) []string {
	if config.ClientSessionCache == nil || config.ServerName == "" {
		return offered
	}
	session, ok := config.ClientSessionCache.Get(config.ServerName)
	if !ok || session == nil {
		return offered
	}
	_, state, err := session.ResumptionState()
	if err != nil || state == nil || !(&handshakeIssuers{available: available}).accepts(state) {
		return offered
	}
	protocol := ""
	for _, entry := range state.Extra {
		if bytes.HasPrefix(entry, []byte(ticketProtocol)) {
			protocol = string(entry[len(ticketProtocol):])
		}
	}
	if protocol == "" {
		return offered
	}
	present := false
	for _, candidate := range offered {
		if candidate == protocol {
			present = true
		}
	}
	if !present {
		return offered
	}
	base := Application(protocol)
	result := make([]string, 0, len(offered))
	for _, candidate := range offered {
		if strings.HasPrefix(candidate, inventory) || Application(candidate) != base || candidate == protocol || candidate == base {
			result = append(result, candidate)
		}
	}
	return result
}
