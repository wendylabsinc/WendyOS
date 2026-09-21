package rtps

import (
	"context"
	"errors"
	"sync"
)

type namespaceIdentity struct{ device, inode uint64 }
type poolKey struct {
	namespace     namespaceIdentity
	iface, domain int
}
type resolvedTarget struct {
	key poolKey
	cfg Config
	ns  *networkNamespace
}

type poolEntry struct {
	ready       chan struct{}
	participant *Participant
	ns          *networkNamespace
	cancel      context.CancelFunc
	done        chan struct{}
	leases      map[*Lease]struct{}
	refs        map[GUID]int
}

// Pool owns one physical participant per namespace, resolved interface and DDS
// domain. It must be closed by its owner after its consumers have shut down.
// An individual lease's cancellation never cancels another consumer's discovery.
type Pool struct {
	mu       sync.Mutex
	entries  map[poolKey]*poolEntry
	closed   bool
	creating sync.WaitGroup
	resolve  func(Config) (*resolvedTarget, error)
	create   func(Config) (*Participant, error)
}

func NewPool() *Pool {
	return &Pool{entries: map[poolKey]*poolEntry{}, resolve: resolveTarget, create: NewParticipant}
}

// Acquire captures and verifies the target namespace before looking up the
// physical participant. Failed/cancelled acquisitions retain neither a reference
// nor a namespace descriptor. Config.Interface is resolved before the lookup.
func (p *Pool) Acquire(ctx context.Context, target Config) (*Lease, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		resolved, err := p.resolve(target)
		if err != nil {
			return nil, err
		}
		p.mu.Lock()
		if p.closed || ctx.Err() != nil {
			p.mu.Unlock()
			resolved.ns.close()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, errors.New("rtps: pool closed")
		}
		entry := p.entries[resolved.key]
		if entry != nil && entry.participant == nil {
			ready := entry.ready
			p.mu.Unlock()
			resolved.ns.close()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ready:
			}
			continue
		}
		if entry != nil {
			// Verify outside the lock: ownership checks call into containerd
			// with no timeout, and every other consumer's subscribe, release
			// and listing waits on this lock.
			p.mu.Unlock()
			err := resolved.ns.verify()
			resolved.ns.close()
			if err != nil {
				return nil, err
			}
			p.mu.Lock()
			if p.closed || ctx.Err() != nil {
				p.mu.Unlock()
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return nil, errors.New("rtps: pool closed")
			}
			if p.entries[resolved.key] != entry {
				// Released or replaced while verifying: look it up again.
				p.mu.Unlock()
				continue
			}
			lease := p.leaseLocked(ctx, resolved.key, entry)
			p.mu.Unlock()
			return lease, nil
		}
		entry = &poolEntry{ready: make(chan struct{}), leases: map[*Lease]struct{}{}, refs: map[GUID]int{}}
		p.entries[resolved.key] = entry
		p.creating.Add(1)
		p.mu.Unlock()

		participant, err := p.create(resolved.cfg)
		if err == nil {
			err = resolved.ns.verify()
		}
		p.mu.Lock()
		if err == nil {
			err = ctx.Err()
		}
		if err == nil && p.closed {
			err = errors.New("rtps: pool closed")
		}
		if err != nil {
			if participant != nil {
				_ = participant.Close()
			}
			resolved.ns.close()
			delete(p.entries, resolved.key)
			close(entry.ready)
			p.mu.Unlock()
			p.creating.Done()
			return nil, err
		}
		entry.participant, entry.ns = participant, resolved.ns
		runCtx, cancel := context.WithCancel(context.Background())
		entry.cancel, entry.done = cancel, make(chan struct{})
		go func() { defer close(entry.done); participant.Run(runCtx) }()
		lease := p.leaseLocked(ctx, resolved.key, entry)
		close(entry.ready)
		p.mu.Unlock()
		p.creating.Done()
		return lease, nil
	}
}

// Lease is a consumer's discovery snapshot and independent bounded sample queue.
// Subscription and Close operations are idempotent. Samples are read-only and may
// share their backing bytes with other consumers of the same writer.
type Lease struct {
	pool    *Pool
	key     poolKey
	entry   *poolEntry
	samples chan Sample
	changed chan struct{}
	done    chan struct{}
	stop    func() bool
	closed  bool              // pool.mu
	subs    map[GUID]struct{} // participant.mu
}

func (p *Pool) leaseLocked(ctx context.Context, key poolKey, entry *poolEntry) *Lease {
	l := &Lease{pool: p, key: key, entry: entry, samples: make(chan Sample, 4), changed: make(chan struct{}, 1), done: make(chan struct{}), subs: map[GUID]struct{}{}}
	entry.leases[l] = struct{}{}
	entry.participant.mu.Lock()
	entry.participant.leases[l] = struct{}{}
	entry.participant.mu.Unlock()
	// A pending notification makes a late consumer inspect the current snapshot.
	select {
	case l.changed <- struct{}{}:
	default:
	}
	l.stop = context.AfterFunc(ctx, func() { _ = l.Close() })
	return l
}

func (l *Lease) Interface() string        { return l.entry.participant.cfg.Interface }
func (l *Lease) Samples() <-chan Sample   { return l.samples }
func (l *Lease) Changed() <-chan struct{} { return l.changed }
func (l *Lease) Done() <-chan struct{}    { return l.done }
func (l *Lease) Endpoints() []Endpoint {
	select {
	case <-l.done:
		return nil
	default:
	}
	return l.entry.participant.Endpoints()
}
func (l *Lease) Stats() Stats { return l.entry.participant.Stats() }

func (l *Lease) Subscribe(ep Endpoint) error {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if l.closed {
		return errors.New("rtps: lease closed")
	}
	p := l.entry.participant
	p.mu.Lock()
	_, already := l.subs[ep.GUID]
	if !already {
		l.subs[ep.GUID] = struct{}{}
	}
	p.mu.Unlock()
	if already {
		return nil
	}
	if l.entry.refs[ep.GUID] == 0 {
		if err := p.Subscribe(ep); err != nil {
			p.mu.Lock()
			delete(l.subs, ep.GUID)
			p.mu.Unlock()
			return err
		}
	}
	l.entry.refs[ep.GUID]++
	return nil
}

func (l *Lease) Unsubscribe(writer GUID) {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	if !l.closed {
		l.unsubscribeLocked(writer)
	}
}

func (l *Lease) unsubscribeLocked(writer GUID) {
	p := l.entry.participant
	p.mu.Lock()
	_, subscribed := l.subs[writer]
	delete(l.subs, writer)
	// Remove queued samples for this writer as well as future delivery.
	for n := len(l.samples); n > 0; n-- {
		select {
		case sample := <-l.samples:
			if sample.Writer != writer {
				enqueueSample(l.samples, sample)
			}
		default:
		}
	}
	p.mu.Unlock()
	if !subscribed {
		return
	}
	l.entry.refs[writer]--
	if l.entry.refs[writer] == 0 {
		delete(l.entry.refs, writer)
		p.Unsubscribe(writer)
	}
}

func (l *Lease) Close() error {
	l.pool.mu.Lock()
	defer l.pool.mu.Unlock()
	l.closeLocked()
	return nil
}

func (l *Lease) closeLocked() {
	if l.closed {
		return
	}
	l.closed = true
	l.stop()
	p := l.entry.participant
	// Only Subscribe and Unsubscribe mutate subs, both under pool.mu.
	for guid := range l.subs {
		l.unsubscribeLocked(guid)
	}
	p.mu.Lock()
	delete(p.leases, l)
	p.mu.Unlock()
	close(l.done)
	delete(l.entry.leases, l)
	if len(l.entry.leases) == 0 {
		l.entry.cancel()
		_ = p.Close()
		<-l.entry.done
		l.entry.ns.close()
		delete(l.pool.entries, l.key)
	}
}

// Participants reports the active physical count for diagnostics.
func (p *Pool) Participants() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, entry := range p.entries {
		if entry.participant != nil {
			n++
		}
	}
	return n
}

func (p *Pool) Close() error {
	p.mu.Lock()
	p.closed = true
	for _, entry := range p.entries {
		for lease := range entry.leases {
			lease.closeLocked()
		}
	}
	p.mu.Unlock()
	p.creating.Wait()
	return nil
}
