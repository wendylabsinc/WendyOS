package meshsession

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"net"
	"testing"
	"time"
)

func directManagerFixture(t *testing.T) (*DirectManager, localmesh.DirectCarrierAdmission) {
	t.Helper()
	a, b := fixtureCredentials(t)
	state, _ := directTestTLSWithCredentials(t, a, b)
	manager, err := NewDirectManager(context.Background(), DirectManagerConfig{Credentials: a, Backend: func(context.Context, meshid.Device, directstream.OpenRequest) (net.Conn, error) {
		return nil, ErrDenied
	}, MaxPeers: 1, RecoveryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.Close)
	return manager, localmesh.DirectCarrierAdmission{Self: a.Device, Peer: b.Device, Role: 0, IdentityDigest: [32]byte{7}, Selected: directstream.CapabilityVersion | directstream.CapabilityTLS, TLS: state, Records: &directTestCarrier{tx: make(chan []byte, 2), rx: make(chan []byte, 2), dropped: make(chan struct{}, 1)}}
}
