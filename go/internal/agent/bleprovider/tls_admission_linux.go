//go:build linux

package bleprovider

import (
	"context"
	"fmt"
	"time"
)

const (
	// An accepted CoC still has the controller's four-second supervision
	// timeout while it waits. Close and retry rather than queueing a handshake
	// behind a second slow peer for most of that interval.
	meshTLSAdmissionWait = 4 * time.Second
	// Once admitted, allow an mTLS certificate flight to progress over a busy
	// multi-ACL adapter. The CoC's write-idle and ACL supervision limits remain
	// independent bounds when the controller stops making progress.
	meshTLSHandshakeTimeout = 15 * time.Second
)

// One gate belongs to one BLE adapter runtime. Both accepted and outgoing
// Wendy CoCs use it, so their large TLS flights cannot all compete for the
// same controller's limited airtime at once. It is released before the
// long-lived mesh stream is attached.
type tlsHandshakeAdmission struct{ slot chan struct{} }

func newTLSHandshakeAdmission() tlsHandshakeAdmission {
	return tlsHandshakeAdmission{slot: make(chan struct{}, 1)}
}

func (g *tlsHandshakeAdmission) run(ctx context.Context, waitBudget, handshakeBudget time.Duration, handshake func(context.Context) error) (wait, duration time.Duration, err error) {
	// Zero value stays usable: struct-literal runtimes in tests never call
	// the constructor, and a nil slot would block admission forever.
	if g.slot == nil {
		g.slot = make(chan struct{}, 1)
	}
	started := time.Now()
	waitCtx, stopWaiting := context.WithTimeout(ctx, waitBudget)
	defer stopWaiting()
	select {
	case g.slot <- struct{}{}:
		defer func() { <-g.slot }()
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
	case <-waitCtx.Done():
		return time.Since(started), 0, fmt.Errorf("BLE TLS admission: %w", waitCtx.Err())
	}
	wait = time.Since(started)
	handshakeCtx, stopHandshake := context.WithTimeout(ctx, handshakeBudget)
	defer stopHandshake()
	started = time.Now()
	err = handshake(handshakeCtx)
	return wait, time.Since(started), err
}
