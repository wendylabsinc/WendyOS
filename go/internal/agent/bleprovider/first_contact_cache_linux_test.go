//go:build linux

package bleprovider

import (
	"crypto/tls"
	"sync"
	"testing"
)

type firstContactCacheRecorder struct {
	mu      sync.Mutex
	puts    int
	key     string
	session *tls.ClientSessionState
}

func (*firstContactCacheRecorder) Get(string) (*tls.ClientSessionState, bool) {
	panic("first contact must not consult a ticket cache")
}
func (c *firstContactCacheRecorder) Put(key string, session *tls.ClientSessionState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.puts++
	c.key, c.session = key, session
}

func TestFirstContactCacheOnlyStoresAfterBinding(t *testing.T) {
	c := &firstContactSessionCache{}
	session := &tls.ClientSessionState{}
	c.Put("untrusted-radio-address", session)
	if got, ok := c.Get("untrusted-radio-address"); got != nil || ok {
		t.Fatal("unidentified peer obtained a ticket")
	}
	target := &firstContactCacheRecorder{}
	c.bind(&tls.Config{ClientSessionCache: target, ServerName: "canonical-peer-name"})
	c.Put("untrusted-radio-address", nil)
	if target.puts != 0 {
		t.Fatal("nil session cleared the verified peer's existing ticket")
	}
	c.Put("untrusted-radio-address", session)
	if target.puts != 1 || target.key != "canonical-peer-name" || target.session != session {
		t.Fatal("ticket did not use the verified peer's canonical cache key")
	}
	if got, ok := c.Get("canonical-peer-name"); got != nil || ok {
		t.Fatal("first-contact config offered a ticket after binding")
	}
}

func TestFirstContactCacheConcurrentCallbacks(t *testing.T) {
	c := &firstContactSessionCache{}
	target := &firstContactCacheRecorder{}
	config := &tls.Config{ClientSessionCache: target, ServerName: "canonical-peer-name"}
	c.bind(config)
	session := &tls.ClientSessionState{}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 32 {
				c.bind(config)
				c.Put("radio", session)
				if got, ok := c.Get("radio"); got != nil || ok {
					t.Error("first contact offered a session")
				}
			}
		}()
	}
	wg.Wait()
	if target.puts != 16*32 || target.key != "canonical-peer-name" {
		t.Fatal("concurrent ticket callbacks lost their scope")
	}
}
