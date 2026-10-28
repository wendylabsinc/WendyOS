package carriers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type observed struct {
	candidate   Candidate
	seen        time.Time
	sources     map[int]bool
	retry       time.Time
	unsupported bool
}
type attempt struct {
	id        uint64
	candidate Candidate
	cancel    context.CancelFunc
	peer      meshid.Device
	verified  bool
	canceled  bool
}
type sourceUpdate struct {
	source int
	items  []Candidate
}
type sourceError struct{ err error }
type authenticated struct {
	key  string
	id   uint64
	peer meshid.Device
}
type completed struct {
	key string
	id  uint64
	err error
}

// Run owns candidate state and connection lifetimes until ctx ends. Source
// disappearance does not close an attached link: BLE commonly stops advertising
// after connection. Only the authenticated node snapshot can supersede it.
func Run(parent context.Context, cfg Config) error {
	if err := cfg.defaults(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	sources := cfg.Sources
	if sources == nil {
		sources = defaultSources(cfg)
	}
	if len(sources) > 16 {
		return fmt.Errorf("too many discovery sources")
	}
	events := make(chan any, 2*cfg.Limits.Connections+2*len(sources)+1)
	if len(cfg.Seeds) != 0 {
		events <- sourceUpdate{source: -1, items: append([]Candidate(nil), cfg.Seeds...)}
	}
	for index, source := range sources {
		if source == nil {
			continue
		}
		wg.Add(1)
		go func(index int, source Source) {
			defer wg.Done()
			for ctx.Err() == nil {
				err := source(ctx, func(items []Candidate) {
					if len(items) > cfg.Limits.Candidates {
						items = items[:cfg.Limits.Candidates]
					}
					copyItems := append([]Candidate(nil), items...)
					select {
					case events <- sourceUpdate{index, copyItems}:
					case <-ctx.Done():
					}
				})
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					select {
					case events <- sourceError{err}:
					case <-ctx.Done():
						return
					}
				}
				timer := time.NewTimer(cfg.Limits.RetryDelay)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return
				}
			}
		}(index, source)
	}
	table := make(map[string]*observed)
	active := make(map[string]*attempt)
	cache := make(peerCache)
	supported := make(map[string]bool)
	for _, transport := range cfg.SupportedTransports {
		supported[transport] = true
	}
	var queue localmesh.CandidateQueue
	var serial uint64
	ticker := time.NewTicker(cfg.Limits.PollInterval)
	defer ticker.Stop()
	snapshot := func() localmesh.NodeSnapshot {
		if cfg.Snapshot != nil {
			return cfg.Snapshot()
		}
		return localmesh.NodeSnapshot{}
	}
	hasLink := func(view localmesh.NodeSnapshot, peer meshid.Device, cost uint16, strict bool) bool {
		if peer == (meshid.Device{}) {
			return false
		}
		for _, link := range view.Links {
			if link.Device == peer && ((strict && link.Cost < cost) || (!strict && link.Cost <= cost)) {
				return true
			}
		}
		return false
	}
	plan := func(now time.Time) {
		view := snapshot()
		for _, a := range active {
			if a.verified && !a.canceled && hasLink(view, a.peer, a.candidate.Cost, true) {
				a.canceled = true
				a.cancel()
			}
		}
		for key, e := range table {
			if !e.sources[-1] && (len(e.sources) == 0 || now.Sub(e.seen) >= cfg.Limits.CandidateTTL) {
				if active[key] == nil {
					delete(table, key)
				}
			}
		}
		keys := make([]string, 0, len(table))
		for key, e := range table {
			if e.sources[-1] || (len(e.sources) != 0 && now.Sub(e.seen) < cfg.Limits.CandidateTTL) {
				keys = append(keys, key)
			}
		}
		for _, key := range queue.Order(keys, now) {
			if len(active) >= cfg.Limits.Connections {
				break
			}
			e := table[key]
			c := e.candidate
			if active[key] != nil || now.Before(e.retry) || !supported[c.Transport] {
				continue
			}
			// An unbound operator hint may share a principal with this installation.
			// Equality includes the actual installation, so other logins remain peers.
			if c.Device == cfg.Credentials.Device {
				continue
			}
			peer := cache.peer(c, now, cfg.Limits.AuthenticatedTTL)
			if hasLink(view, peer, c.Cost, false) {
				continue
			}
			pending, bleCount := 0, 0
			for _, a := range active {
				if !a.verified {
					pending++
				}
				if a.candidate.Transport == BLE {
					bleCount++
				}
			}
			if pending >= cfg.Limits.Attempts || (c.Transport == BLE && bleCount >= cfg.Limits.BLEConnections) {
				continue
			}
			serial++
			child, closeChild := context.WithCancel(ctx)
			a := &attempt{id: serial, candidate: c, cancel: closeChild}
			active[key] = a
			var once sync.Once
			c.authenticated = func(peer meshid.Device) {
				once.Do(func() {
					select {
					case events <- authenticated{key, a.id, peer}:
					case <-child.Done():
					}
				})
			}
			wg.Add(1)
			go func(key string, a *attempt, c Candidate) {
				defer wg.Done()
				err := cfg.Connect(child, c)
				closeChild()
				select {
				case events <- completed{key, a.id, err}:
				case <-ctx.Done():
				}
			}(key, a, c)
		}
	}
	for {
		select {
		case <-ctx.Done():
			return parent.Err()
		case event := <-events:
			now := time.Now()
			switch v := event.(type) {
			case sourceUpdate:
				for _, e := range table {
					delete(e.sources, v.source)
				}
				for _, candidate := range v.items {
					candidate.authenticated = nil
					if !candidate.valid() || !supported[candidate.Transport] {
						continue
					}
					if candidate.Device != (meshid.Device{}) && candidate.Device.Tenant() != cfg.Credentials.Device.Tenant() {
						continue
					}
					key := candidate.key()
					entry := table[key]
					if entry == nil {
						if len(table) >= cfg.Limits.Candidates {
							oldest := ""
							for k, e := range table {
								if active[k] == nil && !e.sources[-1] && (oldest == "" || e.seen.Before(table[oldest].seen) || (e.seen.Equal(table[oldest].seen) && k < oldest)) {
									oldest = k
								}
							}
							if oldest == "" {
								continue
							}
							delete(table, oldest)
						}
						entry = &observed{sources: make(map[int]bool)}
						table[key] = entry
					}
					if entry.unsupported && !entry.candidate.sameAdvertisement(candidate) {
						entry.unsupported = false
						entry.retry = time.Time{}
					}
					entry.candidate = candidate
					entry.seen = now
					entry.sources[v.source] = true
				}
			case authenticated:
				a := active[v.key]
				if a != nil && a.id == v.id && v.peer != (meshid.Device{}) && v.peer != cfg.Credentials.Device && v.peer.Tenant() == cfg.Credentials.Device.Tenant() {
					a.verified = true
					a.peer = v.peer
					cache.remember(a.candidate, v.peer, now, cfg.Limits.AuthenticatedTTL, cfg.Limits.Candidates)
				}
			case completed:
				if a := active[v.key]; a != nil && a.id == v.id {
					delete(active, v.key)
					if e := table[v.key]; e != nil {
						if a.canceled {
							e.retry = now
							queue.Succeeded(v.key)
						} else {
							e.retry = now.Add(cfg.Limits.RetryDelay)
							if errors.Is(v.err, ErrUnsupportedPeer) {
								e.retry = now.Add(15 * time.Minute)
								e.unsupported = true
								if cfg.Status != nil {
									cfg.Status(v.err)
								}
							}
							if v.err != nil {
								queue.Failed(v.key, now)
							} else {
								queue.Succeeded(v.key)
							}
						}
					}
				}
			case sourceError:
				if cfg.Status != nil {
					cfg.Status(v.err)
				}
			}
			plan(now)
		case now := <-ticker.C:
			plan(now)
		}
	}
}
