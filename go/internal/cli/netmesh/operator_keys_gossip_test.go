package netmesh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperatorlink"
)

// Both ordinary logins have independently issued valid primary keys. The
// relay must retain both owners, and each operator must fetch the OTHER key
// through real content-addressed certificate/gossip controls over the relay.
func TestDifferentOperatorKeysGossipAcrossFullRouter(t *testing.T) {
	var authority fixtureAuthority
	fixture := fixtureCredentials(t, &authority)
	first := fixture[1]
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(first.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf.SerialNumber = big.NewInt(777)
	leaf.NotAfter = leaf.NotAfter.Add(-time.Minute)
	der, err := x509.CreateCertificate(rand.Reader, leaf, authority.certificate, key.Public(), authority.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	second, err := localmesh.NewOperatorCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.certificate.Raw})), string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"))
	if err != nil {
		t.Fatal(err)
	}
	credentials := []*localmesh.Credentials{first, fixture[0], second}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	nodes := make([]*localmesh.Node, 3)
	for i, c := range credentials {
		network, err := NewNetwork(c.Device)
		if err != nil {
			t.Fatal(err)
		}
		port := uint16(0)
		if !c.Device.IsOperator() {
			port = 50052
		}
		nodes[i], err = localmesh.NewNodeWithNetwork(ctx, t.TempDir(), c, "normal login", port, network)
		if err != nil {
			_ = network.Close()
			t.Fatal(err)
		}
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			if err := nodes[i].Run(); err != nil && ctx.Err() == nil {
				t.Errorf("node %d: %v", i, err)
			}
		}(i)
	}
	roots := []*x509.Certificate{authority.certificate}
	for _, index := range []int{0, 2} {
		index := index
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		serverConfig, err := credentials[1].PeerTLS(credentials[index].Device)
		if err != nil {
			t.Fatal(err)
		}
		serverConfig.NextProtos = []string{meshoperatorlink.RouterALPN}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer listener.Close()
			stop := context.AfterFunc(ctx, func() { listener.Close() })
			defer stop()
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			secure := tls.Server(raw, serverConfig)
			defer secure.Close()
			if err = secure.HandshakeContext(ctx); err != nil {
				t.Error(err)
				return
			}
			_ = nodes[1].AttachOperatorRouterStream(ctx, secure, 128)
		}()
		clientConfig, err := credentials[index].PeerTLS(credentials[1].Device)
		if err != nil {
			t.Fatal(err)
		}
		clientConfig.NextProtos = []string{meshoperatorlink.RouterALPN}
		secure, err := (&tls.Dialer{Config: clientConfig}).DialContext(ctx, "tcp", listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		identity, err := meshoperator.VerifyPrimary(credentials[index].Certificate.Certificate, roots, credentials[index].Device.Tenant(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		link, err := meshoperatorlink.RouterClient(ctx, secure.(*tls.Conn), meshoperatorlink.ClientConfig{Identity: identity, Signer: credentials[index].Signer, Instance: meshoperator.InstanceID(credentials[index].Device.UUID()), Router: credentials[1].Device, Roots: roots})
		if err != nil {
			t.Fatal(err)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { link.Close(); link.Wait() }()
			_ = nodes[index].AttachOperatorRouterLink(ctx, link, 128)
		}()
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		complete := true
		for _, node := range nodes {
			snapshot := node.Snapshot()
			owners := 0
			for _, m := range snapshot.Devices {
				if m.Owner != nil && m.Owner.Principal == first.Device.Principal() && m.Route != nil && snapshot.RouteAuthorizations[*m.Route] {
					owners++
				}
			}
			complete = complete && owners == 2
		}
		if complete {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("different-key owners did not gossip: first=%+v relay=%+v second=%+v", nodes[0].Snapshot(), nodes[1].Snapshot(), nodes[2].Snapshot())
		case <-ticker.C:
		}
	}
}
