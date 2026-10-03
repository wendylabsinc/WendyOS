package services

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mesh"
	"github.com/wendylabsinc/wendy/go/internal/agent/meshsession"
)

// MeshAppDialer is the opt-in local mesh VIP path. It uses only an end-to-end
// authenticated peer QUIC session. The existing cloud byte relay lacks a
// device-originated app admission protocol and cannot safely carry these
// app flows until that protocol and its service policy are implemented.
type MeshAppDialer struct{ mesh *MeshDialer }

var _ mesh.PeerDialer = (*MeshAppDialer)(nil)

func NewMeshAppDialer(d *MeshDialer) *MeshAppDialer { return &MeshAppDialer{mesh: d} }

func (a *MeshAppDialer) DialDevice(ctx context.Context, deviceID int32, port uint16) (net.Conn, string, error) {
	if a == nil || a.mesh == nil {
		return nil, "", meshsession.ErrNoRoute
	}
	a.mesh.mu.Lock()
	dial := a.mesh.localDial
	a.mesh.mu.Unlock()
	if dial == nil {
		return nil, "", meshsession.ErrNoRoute
	}
	started := a.mesh.now()
	conn, err := dial(ctx, deviceID, port)
	// A Babel route can be replaced while QUIC sends its first packet.
	// Retry one kernel route failure after convergence; never retry an
	// authenticated app-port denial or a missing signed route.
	if errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH) {
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-time.After(time.Second):
			conn, err = dial(ctx, deviceID, port)
		}
	}
	ms := float64(a.mesh.now().Sub(started).Milliseconds())
	if err != nil {
		a.mesh.metrics.RecordDial(deviceID, "local-mesh", "error", ms)
		return nil, "local-mesh", err
	}
	a.mesh.metrics.RecordDial(deviceID, "local-mesh", "ok", ms)
	return conn, "local-mesh", nil
}
