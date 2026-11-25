package backend

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// App controls, inventory reads and pairing scans use the committed device connection
// without waiting behind unrelated camera, installation or cloud requests.
// The connection and session lifetime are immutable until disconnect cancels
// them; gRPC supports concurrent RPCs on this connection.
type deviceRPCSession struct {
	conn *grpcclient.AgentConnection
	life context.Context
	id   uint64
}

// Called with b.mu held when committing the selected device.
func (b *Backend) publishDeviceRPCSession() {
	if b.conn == nil || b.lite != nil {
		b.deviceRPCSession.Store(nil)
		return
	}
	b.deviceRPCSession.Store(&deviceRPCSession{conn: b.conn, life: b.life, id: b.session})
}

func (b *Backend) deviceRPC(session uint64) (context.Context, *grpcclient.AgentConnection, context.CancelFunc, error) {
	snapshot := b.deviceRPCSession.Load()
	if snapshot == nil {
		// Also supports embedded callers that initialize a connection directly.
		b.mu.Lock()
		_, err := b.connection(session)
		if err == nil {
			b.publishDeviceRPCSession()
			snapshot = b.deviceRPCSession.Load()
		}
		b.mu.Unlock()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if session != 0 && snapshot.id != session {
		return nil, nil, nil, errors.New("device session changed")
	}
	ctx, cancel := context.WithTimeout(snapshot.life, 65*time.Second)
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, nil, nil, err
	}
	return ctx, snapshot.conn, cancel, nil
}
