package cloudrelay

import (
	"context"
	"sync"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

const (
	relayPoolLimit  = 8
	relayPoolIdle   = 30 * time.Second
	relayPoolMaxAge = 10 * time.Minute
)

// Incarnations and traffic classes have independent HTTP/2 connections. Grants,
// proofs and stream contexts are never cached. The enclosing Verifier isolates
// issuers and custom browser transports; no process-global connection is shared.
type relayPoolKey struct {
	endpoint, audience string
	role               pb.JoinRole
	management         bool
}

type relayTransport struct {
	key             relayPoolKey
	conn            *grpc.ClientConn
	ready           chan struct{}
	refs            int
	created, idleAt time.Time
	retired         bool
	idleGeneration  uint64
	timer           *time.Timer
}

type relayPool struct {
	mu      sync.Mutex
	entries []*relayTransport
	changed chan struct{}
	closed  bool
	// Tests shorten idle retirement without changing the production bounds.
	idleTimeout time.Duration
}

// Close stops all relay sessions owned by this Verifier and prevents new ones.
// Owners must create a new Verifier when changing accounts or transport policy.
// Presence connections and standalone DialBroker callers retain their ownership.
func (v *Verifier) Close() error {
	v.relays.mu.Lock()
	defer v.relays.mu.Unlock()
	v.relays.closed = true
	for len(v.relays.entries) > 0 {
		v.relays.removeLocked(v.relays.entries[0])
	}
	v.relays.wakeLocked()
	return nil
}

func (p *relayPool) wakeLocked() {
	if p.changed != nil {
		close(p.changed)
	}
	p.changed = make(chan struct{})
}

func (p *relayPool) removeLocked(entry *relayTransport) {
	for i, candidate := range p.entries {
		if candidate == entry {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			break
		}
	}
	entry.retired = true
	if entry.timer != nil {
		entry.timer.Stop()
	}
	if entry.conn != nil {
		_ = entry.conn.Close()
	}
	p.wakeLocked()
}

func relayReusable(entry *relayTransport) bool {
	if entry.retired || time.Since(entry.created) >= relayPoolMaxAge {
		return false
	}
	state := entry.conn.GetState()
	return state != connectivity.TransientFailure && state != connectivity.Shutdown
}

func (p *relayPool) acquire(ctx context.Context, key relayPoolKey, dial func(string) (*grpc.ClientConn, error)) (*grpc.ClientConn, func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		p.mu.Lock()
		if p.changed == nil {
			p.changed = make(chan struct{})
		}
		if p.closed {
			p.mu.Unlock()
			return nil, nil, context.Canceled
		}
		var pending <-chan struct{}
		var idle *relayTransport
		for _, entry := range p.entries {
			if entry.conn == nil {
				if entry.key == key {
					pending = entry.ready
				}
				continue
			}
			if !relayReusable(entry) {
				entry.retired = true
			}
			if entry.key == key && !entry.retired {
				entry.refs++
				entry.idleGeneration++
				if entry.timer != nil {
					entry.timer.Stop()
				}
				p.mu.Unlock()
				return entry.conn, p.release(entry), nil
			}
			if entry.refs == 0 && (idle == nil || entry.idleAt.Before(idle.idleAt)) {
				idle = entry
			}
		}
		if pending != nil {
			changed := p.changed
			p.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-changed:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		if len(p.entries) >= relayPoolLimit && idle != nil {
			p.removeLocked(idle)
		}
		if len(p.entries) >= relayPoolLimit {
			if p.changed == nil {
				p.changed = make(chan struct{})
			}
			changed := p.changed
			p.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
		}
		entry := &relayTransport{key: key, refs: 1, created: time.Now(), ready: make(chan struct{})}
		p.entries = append(p.entries, entry)
		p.mu.Unlock()
		// grpc.NewClient is lazy. Do not hold the pool lock while a custom
		// transport factory runs; other keys and cancelled waiters can proceed.
		conn, err := dial(key.endpoint)
		p.mu.Lock()
		entry.conn = conn
		close(entry.ready)
		if err != nil || p.closed {
			p.removeLocked(entry)
			p.mu.Unlock()
			if err == nil {
				err = context.Canceled
			}
			return nil, nil, err
		}
		p.wakeLocked()
		p.mu.Unlock()
		release := p.release(entry)
		if err := ctx.Err(); err != nil {
			release()
			return nil, nil, err
		}
		return conn, release, nil
	}
}

func (p *relayPool) release(entry *relayTransport) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			entry.refs--
			if entry.refs != 0 {
				return
			}
			if p.closed || !relayReusable(entry) {
				p.removeLocked(entry)
				return
			}
			entry.idleAt = time.Now()
			entry.idleGeneration++
			generation := entry.idleGeneration
			timeout := p.idleTimeout
			if timeout <= 0 {
				timeout = relayPoolIdle
			}
			entry.timer = time.AfterFunc(timeout, func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				if entry.refs == 0 && entry.idleGeneration == generation {
					p.removeLocked(entry)
				}
			})
			p.wakeLocked()
		})
	}
}
