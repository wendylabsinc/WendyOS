package bleprovider

import (
	"sync"
	"time"
)

// globalDialPace is the minimum spacing between outbound BLE dial attempts
// on one device, across all peers. Per-peer backoff already exists; this
// serializes formation and redial bursts that would otherwise start all of
// a device's links (and their catalog sync bursts) in the same seconds.
// Inbound accepts are never paced.
const globalDialPace = 5 * time.Second

// DialPacer shares outbound start spacing across consumers of one adapter.
type DialPacer struct {
	mu   sync.Mutex
	next time.Time
}

func (p *DialPacer) Admit(now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Before(p.next) {
		return false
	}
	p.next = now.Add(globalDialPace)
	return true
}
