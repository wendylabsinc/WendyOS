package localmesh

import (
	"crypto/tls"
	"testing"
	"time"
)

func TestTicketStoreUsesOpaqueBoundedShortLivedTokens(t *testing.T) {
	store := NewTicketStore()
	config := &tls.Config{}
	store.Configure(config)
	if config.SessionTicketsDisabled || config.WrapSession == nil || config.UnwrapSession == nil {
		t.Fatal("ticket hooks not installed")
	}
	state := &tls.SessionState{EarlyData: true}
	var first []byte
	for i := 0; i < meshTicketLimit+2; i++ {
		token, err := config.WrapSession(tls.ConnectionState{}, state)
		if err != nil || len(token) != 32 {
			t.Fatalf("opaque ticket %d: len=%d err=%v", i, len(token), err)
		}
		if i == 0 {
			first = token
		}
	}
	if state.EarlyData {
		t.Fatal("mesh ticket retained early data")
	}
	if len(store.cache.entries) != meshTicketLimit {
		t.Fatalf("ticket count=%d", len(store.cache.entries))
	}
	if resumed, err := config.UnwrapSession(first, tls.ConnectionState{}); err != nil || resumed != nil {
		t.Fatalf("evicted ticket resumed: %v %v", resumed, err)
	}

	now := time.Now()
	store.cache.now = func() time.Time { return now }
	token, err := config.WrapSession(tls.ConnectionState{}, state)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(meshTicketLifetime)
	if resumed, err := config.UnwrapSession(token, tls.ConnectionState{}); err != nil || resumed != nil {
		t.Fatalf("expired ticket resumed: %v %v", resumed, err)
	}
	if _, exists := store.cache.entries[store.key(token, "")]; exists {
		t.Fatal("expired secret retained")
	}
}
