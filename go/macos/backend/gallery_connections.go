package backend

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

const galleryIdleTimeout = 30 * time.Second
const galleryConnectionLimit = 4

type galleryConnections struct {
	mu      sync.Mutex
	entries map[string]*galleryConnection
	blocked bool
	changed chan struct{}
}
type galleryConnection struct {
	ready  chan struct{}
	conn   *grpcclient.AgentConnection
	cancel context.CancelFunc
	users  int
	timer  *time.Timer
}

// Gallery connections are independent of the interactive camera session. Entries
// share a dial, have bounded idle lifetimes, and are discarded on account changes.
func (b *Backend) galleryAcquire(ctx context.Context, selector string) (*grpcclient.AgentConnection, func(error), error) {
	p := &b.gallery
	for {
		p.mu.Lock()
		if p.changed == nil {
			p.changed = make(chan struct{})
		}
		if p.blocked {
			p.mu.Unlock()
			return nil, nil, errors.New("gallery session is inactive")
		}
		if e := p.entries[selector]; e != nil {
			if e.conn == nil {
				ready := e.ready
				p.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				case <-ready:
					continue
				}
			}
			if e.timer != nil {
				e.timer.Stop()
				e.timer = nil
			}
			e.users++
			p.mu.Unlock()
			return e.conn, b.galleryRelease(selector, e), nil
		}
		if p.entries == nil {
			p.entries = make(map[string]*galleryConnection)
		}
		if len(p.entries) >= galleryConnectionLimit {
			// Evict only idle entries; never interrupt another device's download.
			var evicted bool
			for key, e := range p.entries {
				if e.conn != nil && e.users == 0 {
					b.galleryRemoveLocked(key, e)
					evicted = true
					break
				}
			}
			if !evicted {
				changed := p.changed
				p.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				case <-changed:
					continue
				}
			}
		}
		life, cancel := context.WithCancel(context.Background())
		e := &galleryConnection{ready: make(chan struct{}), cancel: cancel, users: 1}
		p.entries[selector] = e
		p.mu.Unlock()
		// Bound setup, but keep the transport lifetime independent of the first RPC.
		dialCtx, dialCancel := context.WithTimeout(life, 20*time.Second)
		stop := context.AfterFunc(ctx, dialCancel)
		conn, lite, err := b.connectTarget(dialCtx, selector)
		stop()
		// Agent transports do not retain their dial context (Lite transports do).
		dialCancel()
		if lite != nil {
			lite.Close()
		}
		if err == nil && conn == nil {
			err = errors.New("recordings require a WendyOS agent")
		}
		p.mu.Lock()
		if p.entries[selector] != e || p.blocked || ctx.Err() != nil {
			if err == nil {
				err = context.Canceled
			}
		}
		if err != nil {
			if p.entries[selector] == e {
				delete(p.entries, selector)
				if p.changed != nil {
					close(p.changed)
					p.changed = make(chan struct{})
				}
			}
			cancel()
			close(e.ready)
			p.mu.Unlock()
			if conn != nil {
				conn.Close()
			}
			return nil, nil, err
		}
		e.conn = conn
		close(e.ready)
		p.mu.Unlock()
		return conn, b.galleryRelease(selector, e), nil
	}
}

func (b *Backend) galleryRelease(selector string, e *galleryConnection) func(error) {
	var once sync.Once
	return func(err error) {
		once.Do(func() {
			p := &b.gallery
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.entries[selector] != e {
				return
			}
			e.users--
			if p.changed != nil {
				close(p.changed)
				p.changed = make(chan struct{})
			}
			if err != nil {
				b.galleryRemoveLocked(selector, e)
				return
			}
			if e.users == 0 {
				e.timer = time.AfterFunc(galleryIdleTimeout, func() {
					p.mu.Lock()
					defer p.mu.Unlock()
					if p.entries[selector] == e && e.users == 0 {
						b.galleryRemoveLocked(selector, e)
					}
				})
			}
		})
	}
}
func (b *Backend) galleryRemoveLocked(selector string, e *galleryConnection) {
	delete(b.gallery.entries, selector)
	if b.gallery.changed != nil {
		close(b.gallery.changed)
		b.gallery.changed = make(chan struct{})
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.cancel()
	// Close may wait on network shutdown; never hold the pool lock while it does.
	if e.conn != nil {
		go e.conn.Close()
	}
}
func (b *Backend) galleryReset(blocked bool) {
	p := &b.gallery
	p.mu.Lock()
	p.blocked = blocked
	var connections []*grpcclient.AgentConnection
	for key, e := range p.entries {
		delete(p.entries, key)
		if e.timer != nil {
			e.timer.Stop()
		}
		e.cancel()
		if e.conn != nil {
			connections = append(connections, e.conn)
		}
	}
	if p.changed != nil {
		close(p.changed)
		p.changed = make(chan struct{})
	}
	p.mu.Unlock()
	// Finish transport closure before the caller tears down the account router.
	for _, conn := range connections {
		conn.Close()
	}
}
