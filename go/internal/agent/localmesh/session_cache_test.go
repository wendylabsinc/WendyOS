package localmesh

import (
	"crypto/tls"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestAggregateSessionCacheBytesCountRecencyAndExpiry(t *testing.T) {
	c := newMeshSessionCache()
	c.maxBytes = 12
	c.maxEntries = 2
	now := time.Now()
	c.now = func() time.Time { return now }
	a, b, d := sessionKey{key: "a"}, sessionKey{key: "b", client: true}, sessionKey{key: "d"}
	c.put(a, []byte("1234"), nil)
	c.put(b, []byte("5678"), nil)
	if _, _, ok := c.get(a); !ok {
		t.Fatal("missing first")
	}
	c.put(d, []byte("abcd"), nil)
	if _, _, ok := c.get(b); ok {
		t.Fatal("least recent client entry not evicted across roles")
	}
	if c.bytes != 10 || len(c.entries) != 2 {
		t.Fatalf("budget bytes=%d count=%d", c.bytes, len(c.entries))
	}
	c.put(a, []byte("12345678"), nil)
	if _, _, ok := c.get(d); ok {
		t.Fatal("byte budget did not evict server entry")
	}
	now = now.Add(meshTicketLifetime - 1)
	if _, _, ok := c.get(a); !ok {
		t.Fatal("premature expiry")
	}
	now = now.Add(1)
	if _, _, ok := c.get(a); ok {
		t.Fatal("access extended ticket lifetime")
	}
	if c.bytes != 0 || len(c.entries) != 0 {
		t.Fatal("expiry retained entry")
	}
}

func TestRetainedTLSConfigsShareBudgetAndCannotReviveEvictedTickets(t *testing.T) {
	c := newMeshSessionCache()
	c.maxEntries = 2
	configs := make([]*tls.Config, 3)
	tokens := make([][]byte, 3)
	for i := range configs {
		configs[i] = &tls.Config{}
		(&TicketStore{cache: c, scope: fmt.Sprint(i)}).Configure(configs[i])
		var err error
		tokens[i], err = configs[i].WrapSession(tls.ConnectionState{}, &tls.SessionState{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if state, err := configs[0].UnwrapSession(tokens[0], tls.ConnectionState{}); state != nil || err != nil {
		t.Fatal("old config bypassed aggregate eviction")
	}
	if state, err := configs[1].UnwrapSession(tokens[2], tls.ConnectionState{}); state != nil || err != nil {
		t.Fatal("cross-scope server ticket accepted")
	}
	if len(c.entries) != 2 {
		t.Fatal("wrong aggregate count")
	}
}

func TestScopedClientCacheCopiesAndInvalidatesState(t *testing.T) {
	_, _, session := ticketFixture(t)
	c := newMeshSessionCache()
	a := scopedClientSessionCache{cache: c, scope: "peer/ble"}
	b := scopedClientSessionCache{cache: c, scope: "peer/quic"}
	a.Put("server", session)
	if _, ok := b.Get("server"); ok {
		t.Fatal("cross-transport ticket accepted")
	}
	one, ok := a.Get("server")
	if !ok {
		t.Fatal("ticket missing")
	}
	token, state, err := one.ResumptionState()
	if err != nil {
		t.Fatal(err)
	}
	token[0] ^= 255
	state.Extra = append(state.Extra, []byte("mutation"))
	two, ok := a.Get("server")
	if !ok {
		t.Fatal("second lookup missing")
	}
	token2, state2, err := two.ResumptionState()
	if err != nil {
		t.Fatal(err)
	}
	if token[0] == token2[0] || len(state.Extra) == len(state2.Extra) {
		t.Fatal("returned session aliases cache")
	}
	a.Put("server", nil)
	if _, ok := a.Get("server"); ok {
		t.Fatal("nil Put did not invalidate")
	}
}

func TestAggregateSessionCacheConcurrentScopesStayBounded(t *testing.T) {
	c := newMeshSessionCache()
	c.maxBytes = 4096
	c.maxEntries = 32
	var wg sync.WaitGroup
	for peer := 0; peer < 16; peer++ {
		wg.Add(1)
		go func(peer int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				key := sessionKey{scope: fmt.Sprint(peer), key: fmt.Sprint(n), client: n%2 == 0}
				c.put(key, make([]byte, 128), nil)
				c.get(key)
			}
		}(peer)
	}
	wg.Wait()
	if c.bytes > c.maxBytes || len(c.entries) > c.maxEntries {
		t.Fatal("aggregate budget exceeded")
	}
}

func TestParsedPeerConfigEvictionKeepsRecentlyUsedIdentity(t *testing.T) {
	creds, _ := testCredentials(t)
	first, _ := creds.PeerTLS(1000)
	cold, _ := creds.PeerTLS(1001)
	for peer := int32(1002); peer < 1256; peer++ {
		if _, err := creds.PeerTLS(peer); err != nil {
			t.Fatal(err)
		}
	}
	creds.PeerTLS(1000)
	creds.PeerTLS(1256)
	hot, _ := creds.PeerTLS(1000)
	evicted, _ := creds.PeerTLS(1001)
	if &first.Certificates[0].Certificate[0][0] != &hot.Certificates[0].Certificate[0][0] {
		t.Fatal("recent config evicted")
	}
	if &cold.Certificates[0].Certificate[0][0] == &evicted.Certificates[0].Certificate[0][0] {
		t.Fatal("cold config retained beyond capacity")
	}
}

func TestSharedListenersUseCredentialBudgetAcrossInstancesAndProtocols(t *testing.T) {
	creds, _ := testCredentials(t)
	memory := creds.ticketCache()
	memory.maxEntries = 2
	a, b, c := &tls.Config{}, &tls.Config{}, &tls.Config{}
	creds.ServerTicketStore("ble/1", "ble").Configure(a)
	creds.ServerTicketStore("catalog/2", "catalog").Configure(b)
	creds.ServerTicketStore("app/1", "quic").Configure(c)
	first, _ := a.WrapSession(tls.ConnectionState{}, &tls.SessionState{})
	other, _ := b.WrapSession(tls.ConnectionState{}, &tls.SessionState{})
	_, _ = c.WrapSession(tls.ConnectionState{}, &tls.SessionState{})
	if s, e := a.UnwrapSession(first, tls.ConnectionState{}); s != nil || e != nil {
		t.Fatal("retained listener bypassed shared budget")
	}
	if s, e := a.UnwrapSession(other, tls.ConnectionState{}); s != nil || e != nil {
		t.Fatal("listener reused another protocol ticket")
	}
	recreated := creds.ServerTicketStore("catalog/2", "catalog")
	if recreated.cache != memory || recreated.scope != creds.ServerTicketStore("catalog/2", "catalog").scope {
		t.Fatal("new listener lost credential scope")
	}
	if len(memory.entries) != 2 {
		t.Fatal("listeners allocated private stores")
	}
}

func TestSharedListenerBindsTicketToNegotiatedALPN(t *testing.T) {
	creds, state, _ := ticketFixture(t)
	cfg := &tls.Config{}
	creds.ServerTicketStore("catalog", "tls").Configure(cfg)
	token, err := cfg.WrapSession(tls.ConnectionState{NegotiatedProtocol: "catalog/1"}, state)
	if err != nil {
		t.Fatal(err)
	}
	if resumed, err := cfg.UnwrapSession(token, tls.ConnectionState{NegotiatedProtocol: "catalog/2"}); resumed != nil || err != nil {
		t.Fatal("cross-version listener ticket resumed")
	}
	if resumed, err := cfg.UnwrapSession(token, tls.ConnectionState{NegotiatedProtocol: "catalog/1"}); resumed == nil || err != nil {
		t.Fatalf("same-version ticket missed: %v", err)
	}
}
