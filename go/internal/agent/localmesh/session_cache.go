package localmesh

import (
	"container/list"
	"crypto/tls"
	"sync"
	"time"
)

const (
	// One budget covers server and client states across all peers, ALPNs and
	// carriers. At 12 KiB/state, 150 peers × four protocols × both roles fit.
	// Serialized payload/key bytes are bounded separately from small metadata
	// (at most 2048 entries). Parsed certificate graphs are not retained here.
	meshSessionCacheBytes   = 16 << 20
	meshSessionCacheEntries = 2048
)

type sessionKey struct {
	scope, key string
	client     bool
}
type sessionEntry struct {
	key           sessionKey
	state, ticket []byte
	created       time.Time
}

func (e *sessionEntry) size() int {
	return len(e.state) + len(e.ticket) + len(e.key.scope) + len(e.key.key)
}

type meshSessionCache struct {
	mu                          sync.Mutex
	entries                     map[sessionKey]*list.Element
	recent                      list.List
	bytes, maxBytes, maxEntries int
	now                         func() time.Time
}

func newMeshSessionCache() *meshSessionCache {
	return &meshSessionCache{entries: make(map[sessionKey]*list.Element), maxBytes: meshSessionCacheBytes, maxEntries: meshSessionCacheEntries, now: time.Now}
}
func (c *meshSessionCache) remove(e *list.Element) {
	item := e.Value.(*sessionEntry)
	delete(c.entries, item.key)
	c.bytes -= item.size()
	c.recent.Remove(e)
}
func (c *meshSessionCache) put(key sessionKey, state, ticket []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.entries[key]; old != nil {
		c.remove(old)
	}
	if state == nil {
		return
	}
	now := c.now()
	for e := c.recent.Back(); e != nil; {
		prev := e.Prev()
		if now.Sub(e.Value.(*sessionEntry).created) >= meshTicketLifetime {
			c.remove(e)
		}
		e = prev
	}
	item := &sessionEntry{key: key, state: state, ticket: ticket, created: now}
	if item.size() > c.maxBytes || len(state) > maxTicketStateBytes {
		return
	}

	// Client configs need at most two server-name entries per scope. Shared
	// inbound listeners serve many assets and use the aggregate server limit.
	if key.client {
		count := 0
		for e := c.recent.Front(); e != nil; {
			next := e.Next()
			other := e.Value.(*sessionEntry)
			if other.key.scope == key.scope && other.key.client {
				count++
				if count >= 2 {
					c.remove(e)
				}
			}
			e = next
		}
	}

	for c.recent.Len() >= c.maxEntries || c.bytes+item.size() > c.maxBytes {
		c.remove(c.recent.Back())
	}
	// Copy external buffers: callers cannot mutate cached secrets, nor retain a
	// huge backing array through a small visible ticket slice.
	item.state = append([]byte(nil), state...)
	item.ticket = append([]byte(nil), ticket...)
	c.entries[key] = c.recent.PushFront(item)
	c.bytes += item.size()
}
func (c *meshSessionCache) get(key sessionKey) (state, ticket []byte, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.entries[key]
	if e == nil {
		return nil, nil, false
	}
	item := e.Value.(*sessionEntry)
	if c.now().Sub(item.created) >= meshTicketLifetime {
		c.remove(e)
		return nil, nil, false
	}
	c.recent.MoveToFront(e)
	// Only this package's parsing hooks consume these immutable buffers.
	return item.state, item.ticket, true
}

type scopedClientSessionCache struct {
	cache *meshSessionCache
	scope string
}

func (s scopedClientSessionCache) Put(key string, session *tls.ClientSessionState) {
	cacheKey := sessionKey{scope: s.scope, key: key, client: true}
	if session == nil {
		s.cache.put(cacheKey, nil, nil)
		return
	}
	ticket, state, err := session.ResumptionState()
	if err != nil {
		s.cache.put(cacheKey, nil, nil)
		return
	}
	encoded, err := state.Bytes()
	if err != nil {
		s.cache.put(cacheKey, nil, nil)
		return
	}
	s.cache.put(cacheKey, encoded, ticket)
}
func (s scopedClientSessionCache) Get(key string) (*tls.ClientSessionState, bool) {
	encoded, ticket, ok := s.cache.get(sessionKey{scope: s.scope, key: key, client: true})
	if !ok {
		return nil, false
	}
	state, err := tls.ParseSessionState(append([]byte(nil), encoded...))
	if err != nil {
		return nil, false
	}
	session, err := tls.NewResumptionState(append([]byte(nil), ticket...), state)
	return session, err == nil
}
