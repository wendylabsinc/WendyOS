package localmesh

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"time"
)

const (
	meshTicketLifetime  = 10 * time.Minute
	meshTicketLimit     = 128
	maxTicketStateBytes = 256 << 10
)

// TicketStore holds TLS session secrets only in memory for one credentials,
// ALPN and transport scope. A random 256-bit wire handle replaces Go's usual
// large self-contained ticket, which otherwise repeats both certificate
// chains in the resumed ClientHello. Tickets never survive agent restart.
type TicketStore struct {
	cache *meshSessionCache
	scope string
}

func NewTicketStore() *TicketStore {
	cache := newMeshSessionCache()
	cache.maxEntries = meshTicketLimit
	return &TicketStore{cache: cache}
}

func (s *TicketStore) Configure(config *tls.Config) {
	config.SessionTicketsDisabled = false
	config.WrapSession = func(connection tls.ConnectionState, state *tls.SessionState) ([]byte, error) {
		state.EarlyData = false // mesh never accepts 0-RTT application data
		encoded, err := state.Bytes()
		if err != nil {
			return nil, err
		}
		if len(encoded) > maxTicketStateBytes {
			return nil, errors.New("oversized mesh TLS session state")
		}
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, err
		}
		s.cache.put(s.key(token[:], connection.NegotiatedProtocol), encoded, nil)
		return token[:], nil
	}
	config.UnwrapSession = func(token []byte, connection tls.ConnectionState) (*tls.SessionState, error) {
		if len(token) != 32 {
			return nil, nil
		}
		encoded, _, exists := s.cache.get(s.key(token, connection.NegotiatedProtocol))
		if !exists {
			return nil, nil // expired/unknown token falls back to full mTLS
		}
		return tls.ParseSessionState(append([]byte(nil), encoded...))
	}
}

func (s *TicketStore) key(token []byte, alpn string) sessionKey {
	// A listener may offer protocol versions side by side. Bind each ticket to
	// the negotiated ALPN as well as the caller's peer/transport scope.
	return sessionKey{scope: fmt.Sprintf("%s/alpn/%d:%s", s.scope, len(alpn), alpn), key: string(token)}
}
