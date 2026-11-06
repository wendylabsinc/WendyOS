//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

type acceptedRadioConn struct {
	net.Conn
	radio l2addr
}

func (c *acceptedRadioConn) RemoteAddr() net.Addr { return c.radio }

func TestAuthenticatedInboundRadioSuppressesEqualHintRedial(t *testing.T) {
	serverConfig, clientConfig := metricsTLSConfigs(t)
	higher := selfFixture.UUID()
	higher[15]++
	peer := meshid.NewDevice(testTenant, higher)
	credentials := &localmesh.Credentials{Device: selfFixture, Certificate: serverConfig.Certificates[0], Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
		if len(chain) != 1 || string(chain[0]) != string(clientConfig.Certificates[0].Certificate[0]) {
			return localmesh.Identity{}, errors.New("untrusted fixture certificate")
		}
		return localmesh.Identity{Device: peer}, nil
	}}
	node := &refreshTestNode{attached: make(chan struct{})}
	r := &runtime{cfg: Config{Credentials: credentials, Node: node, Logger: zap.NewNop()},
		advertisementRefresh: make(chan struct{}, 1)}
	r.serverTLS = r.makeServerTLS()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	radio := l2addr{Address: "AA:BB:CC:DD:EE:01", Type: btAddrLEPublic, PSM: DefaultPSM}
	done := make(chan struct{})
	go func() { r.acceptLink(ctx, &acceptedRadioConn{Conn: a, radio: radio}); close(done) }()
	secure := tls.Client(b, clientConfig)
	if err := secure.HandshakeContext(ctx); err != nil {
		t.Fatal(err)
	}
	clientDone := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, secure); close(clientDone) }()
	select {
	case <-node.attached:
	case <-ctx.Done():
		t.Fatal("inbound fixture did not authenticate and attach")
	}
	candidate := candidate{hint: deviceHint(higher), address: radio.Address, addressType: "public", psm: DefaultPSM}

	if l := r.reserveCandidate(candidate); l != nil {
		l.Release()
		t.Error("already authenticated preferred inbound reserved another dial slot")
	}
	cancel()
	_ = b.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inbound worker did not stop")
	}
	<-clientDone
	// A remembered radio is not permanent authorization or a direction ban.
	// After the active link and cooldown end, certificate/UUID rotation must
	// still reach authentication, even if its advertisement prefix is unchanged.
	l := r.reserveCandidate(candidate)
	if l == nil {
		t.Fatal("inactive remembered radio could not reauthenticate")
	}
	l.Release()

}
