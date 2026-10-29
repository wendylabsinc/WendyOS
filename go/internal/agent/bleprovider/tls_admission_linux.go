//go:build linux

package bleprovider

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	// A busy adapter may still be authenticating another neighbour. Let its
	// bounded accepted-CoC queue wait for that certificate flight instead of
	// repeatedly closing and recreating radio connections. Link-layer
	// supervision is independent of this application admission wait.
	meshTLSAdmissionWait = 30 * time.Second
	// Once admitted, allow an mTLS certificate flight to progress over a busy
	// multi-ACL adapter. The CoC's write-idle and ACL supervision limits remain
	// independent bounds when the controller stops making progress.
	meshTLSHandshakeTimeout = 30 * time.Second
	// A client cannot observe remote admission. Its total budget covers that
	// wait followed by the peer's bounded authentication flight. Runtime or
	// caller cancellation still interrupts either phase immediately.
	meshTLSOutboundTimeout = meshTLSAdmissionWait + meshTLSHandshakeTimeout
)

// One gate belongs to one BLE adapter runtime's accepted CoCs. Outgoing setup
// holds dialSlot from before socket creation through authentication, so it
// cannot queue a remote inbound flight before reserving its own setup slot.
// Separate admission allows progress when peers dial each other concurrently;
// at most one inbound and one outbound handshake use the adapter at a time.
type tlsHandshakeAdmission struct {
	once sync.Once
	slot chan struct{}
}

func newTLSHandshakeAdmission() tlsHandshakeAdmission {
	return tlsHandshakeAdmission{slot: make(chan struct{}, 1)}
}

func (g *tlsHandshakeAdmission) run(ctx context.Context, waitBudget, handshakeBudget time.Duration, handshake func(context.Context) error) (wait, duration time.Duration, err error) {
	// Zero value stays usable: struct-literal runtimes in tests never call
	// the constructor, and a nil slot would block admission forever.
	g.once.Do(func() {
		if g.slot == nil {
			g.slot = make(chan struct{}, 1)
		}
	})
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
