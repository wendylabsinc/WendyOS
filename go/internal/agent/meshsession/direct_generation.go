package meshsession

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// CarrierGeneration binds one configured mesh runtime to this application
// owner. Release revokes its retained epochs, including promoted connections.
// It is not called for ordinary physical carrier loss. A stale handler cannot
// admit a new epoch after release, even if a later node uses the same identity.
func (m *DirectManager) CarrierGeneration() (localmesh.DirectCarrierHandler, func(), error) {
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return nil, nil, net.ErrClosed
	}
	if m.carrierBound {
		m.mu.Unlock()
		return nil, nil, errors.New("direct manager already bound to a mesh generation")
	}
	m.carrierBound = true
	m.mu.Unlock()
	var gate sync.RWMutex
	var handlers sync.WaitGroup
	generationCtx, stopGeneration := context.WithCancel(m.ctx)
	active := true
	handler := func(ctx context.Context, a localmesh.DirectCarrierAdmission) error {
		gate.RLock()
		if !active {
			gate.RUnlock()
			return net.ErrClosed
		}
		handlers.Add(1)
		gate.RUnlock()
		defer handlers.Done()
		ctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(generationCtx, cancel)
		defer cancel()
		defer stop()
		// The manager owns parked lanes and successive application epochs.
		// Attaching directly to one session bypasses deferred hello admission
		// and treats an ordinary promoted carrier replacement as an error.
		return m.HandleCarrier(ctx, a)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			gate.Lock()
			defer gate.Unlock()
			active = false
			stopGeneration()
			// Join parked/proposing handlers before collecting sessions: none
			// may install a fresh epoch after this generation is released.
			handlers.Wait()
			m.mu.Lock()
			sessions := make([]*DirectSession, 0, len(m.peers))
			for _, entry := range m.peers {
				sessions = append(sessions, entry.session)
			}
			m.peers = make(map[meshid.Device]*managedDirectSession)
			m.mu.Unlock()
			for _, session := range sessions {
				session.Close()
			}
			m.mu.Lock()
			m.carrierBound = false
			m.mu.Unlock()
		})
	}
	return handler, release, nil
}
