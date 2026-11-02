//go:build linux

package netmesh

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

// Exercise the production native listener with a caller-owned packet network;
// no kernel interface or privilege is needed for real operator mTLS/admission.
func TestNativeTLSListenerAdmitsFullPrimaryOperatorRouter(t *testing.T) {
	var authority fixtureAuthority
	credentials := fixtureCredentials(t, &authority)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	network, err := NewNetwork(credentials[0].Device)
	if err != nil {
		t.Fatal(err)
	}
	node, err := localmesh.NewNodeWithNetwork(ctx, t.TempDir(), credentials[0], "listener", 50052, network)
	if err != nil {
		_ = network.Close()
		t.Fatal(err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- node.Run() }()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		cancel()
		<-runDone
		t.Fatal(err)
	}
	listenDone := make(chan error, 1)
	go func() { listenDone <- localmesh.RunTLSListener(ctx, node, listener, 128) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		select {
		case <-listenDone:
		case <-time.After(3 * time.Second):
			t.Error("native listener did not drain")
		}
		select {
		case <-runDone:
		case <-time.After(3 * time.Second):
			t.Error("native Node did not stop")
		}
	})
	identity, err := meshoperator.VerifyPrimary(credentials[1].Certificate.Certificate, []*x509.Certificate{authority.certificate}, credentials[0].Device.Tenant(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	config := meshoperatorlink.ClientConfig{Identity: identity, Signer: credentials[1].Signer, Instance: meshoperator.InstanceID(credentials[1].Device.UUID()), Router: credentials[0].Device, Roots: []*x509.Certificate{authority.certificate}}
	tlsConfig, err := meshoperatorlink.ClientTLSConfig(config, credentials[1].Certificate)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	link, err := meshoperatorlink.RouterClient(ctx, raw.(*tls.Conn), config)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	defer func() { _ = link.Close(); link.Wait() }()
	for {
		snapshot := node.Snapshot()
		if len(snapshot.Links) == 1 {
			if snapshot.Links[0].Device != credentials[1].Device {
				t.Fatal("listener lost the primary operator installation")
			}
			break
		}
		select {
		case err := <-listenDone:
			listenDone <- err
			t.Fatal("native listener ended", err)
		case <-ctx.Done():
			t.Fatal("native listener did not attach the full router")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	if _, err := link.ReceiveDatagram(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Log("carrier stopped:", err)
	}
}
