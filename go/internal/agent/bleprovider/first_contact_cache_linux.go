//go:build linux

package bleprovider

import (
	"crypto/tls"
	"sync"
)

// firstContactSessionCache collects tickets without ever offering one on an
// unidentified radio connection. After certificate and ALPN verification, bind
// redirects tickets to the same principal-scoped cache/key used by reconnects.
// It retains no session secrets outside the credentials-wide bounded cache.
type firstContactSessionCache struct {
	mu         sync.Mutex
	cache      tls.ClientSessionCache
	serverName string
}

func (*firstContactSessionCache) Get(string) (*tls.ClientSessionState, bool) {
	return nil, false
}

func (c *firstContactSessionCache) Put(_ string, session *tls.ClientSessionState) {
	if session == nil {
		return
	}
	c.mu.Lock()
	cache, name := c.cache, c.serverName
	c.mu.Unlock()
	if cache != nil {
		cache.Put(name, session)
	}
}

func (c *firstContactSessionCache) bind(config *tls.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cache := config.ClientSessionCache
	for {
		wrapper, ok := cache.(interface{ Underlying() tls.ClientSessionCache })
		if !ok {
			break
		}
		cache = wrapper.Underlying()
	}
	// This collector is itself wrapped by the live handshake. Do not retain
	// snapshots from the unhandshaken config used only to obtain its cache key.
	c.cache, c.serverName = cache, config.ServerName
}
