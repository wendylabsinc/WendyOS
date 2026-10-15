package localmesh

import (
	"crypto/rand"
	"crypto/tls"
	"errors"
	"sync"
	"time"
)

const (
	meshTicketLifetime  = 10 * time.Minute
	meshTicketLimit     = 128
	maxTicketStateBytes = 256 << 10
)

type ticketEntry struct {
	state   []byte
	created time.Time
}

// TicketStore holds TLS session secrets only in memory for one credentials,
// ALPN and transport scope. A random 256-bit wire handle replaces Go's usual
// large self-contained ticket, which otherwise repeats both certificate
// chains in the resumed ClientHello. Tickets never survive agent restart.
type TicketStore struct {
	mu      sync.Mutex
	entries map[[32]byte]ticketEntry
}

func NewTicketStore() *TicketStore {
	return &TicketStore{entries: make(map[[32]byte]ticketEntry)}
}

func (s *TicketStore) Configure(config *tls.Config) {
	config.SessionTicketsDisabled = false
	config.WrapSession = func(_ tls.ConnectionState, state *tls.SessionState) ([]byte, error) {
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
		now := time.Now()
		s.mu.Lock()
		defer s.mu.Unlock()
		for key, entry := range s.entries {
			if now.Sub(entry.created) >= meshTicketLifetime {
				delete(s.entries, key)
			}
		}
		if len(s.entries) >= meshTicketLimit {
			var oldest [32]byte
			var when time.Time
			for key, entry := range s.entries {
				if when.IsZero() || entry.created.Before(when) {
					oldest, when = key, entry.created
				}
			}
			delete(s.entries, oldest)
		}
		s.entries[token] = ticketEntry{state: encoded, created: now}
		return token[:], nil
	}
	config.UnwrapSession = func(token []byte, _ tls.ConnectionState) (*tls.SessionState, error) {
		if len(token) != 32 {
			return nil, nil
		}
		var key [32]byte
		copy(key[:], token)
		now := time.Now()
		s.mu.Lock()
		entry, exists := s.entries[key]
		if exists && now.Sub(entry.created) >= meshTicketLifetime {
			delete(s.entries, key)
			exists = false
		}
		s.mu.Unlock()
		if !exists {
			return nil, nil // expired/unknown token falls back to full mTLS
		}
		return tls.ParseSessionState(entry.state)
	}
}
