package localmesh

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
)

type crlLink struct {
	nodeLink
	closed bool
}

func (l *crlLink) Close() error              { l.closed = true; return nil }
func (l *crlLink) SendDatagram([]byte) error { return nil }

type crlTUN struct{}

func (crlTUN) Read([]byte) (int, error)    { return 0, io.EOF }
func (crlTUN) Write(b []byte) (int, error) { return len(b), nil }
func (crlTUN) Close() error                { return nil }

type crlNetwork struct {
	lifecyclePacketNetwork
	removed int
}

func (*crlNetwork) AddPeer(babel.LinkID, meshid.Device) (io.ReadWriteCloser, error) {
	return crlTUN{}, nil
}
func (n *crlNetwork) RemovePeer(babel.LinkID) error { n.removed++; return nil }

func TestCRLRefreshRemovesMeshCarrierAndCachedRoutes(t *testing.T) {
	for _, operator := range []bool{false, true} {
		t.Run(map[bool]string{false: "device", true: "operator"}[operator], func(t *testing.T) {
			p := crltest.New(t, true)
			previous := revocation.Default
			revocation.Default = revocation.New(p.Client)
			defer func() { revocation.Default = previous }()
			own := p.Issue(t, 10, testPrincipal(testTenantUUID, testDeviceAID))
			ownCreds, err := NewCredentials(own.PEM, p.PEM, own.KeyPEM)
			if err != nil {
				t.Fatal(err)
			}
			principal := testPrincipal(testTenantUUID, testDeviceBID)
			if operator {
				principal = "spiffe://wendy.sh/tenant/" + testTenantUUID + "/operator/alice"
			}
			remote := p.Issue(t, 42, principal)
			var peerCreds *Credentials
			if operator {
				peerCreds, err = NewOperatorCredentials(remote.PEM, p.PEM, remote.KeyPEM, uuid.New())
			} else {
				peerCreds, err = NewCredentials(remote.PEM, p.PEM, remote.KeyPEM)
			}
			if err != nil {
				t.Fatal(err)
			}
			backend := &crlNetwork{}
			node, err := NewNodeWithNetwork(context.Background(), t.TempDir(), ownCreds, "crl", 50052, backend)
			if err != nil {
				t.Fatal(err)
			}
			defer node.cancel()
			defer node.Stop()
			now := time.Now()
			if _, err = node.cache.Put(peerCreds.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			if !operator {
				manifest := routeManifest(peerCreds.Device, now)
				wire, err := SignManifest(manifest, peerCreds.Certificate.Certificate, peerCreds.Signer, now)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = node.directory.Accept(wire, now); err != nil {
					t.Fatal(err)
				}
				node.refresh()
				if !node.Snapshot().RouteAuthorizations[*manifest.Route] {
					t.Fatal("missing initial route authorization")
				}
			}
			link := &crlLink{}
			peer := &nodePeer{device: peerCreds.Device, cost: 256, link: link, authenticatedChain: [][]byte{remote.Cert.Raw}, control: make(chan ControlMessage, 128), done: make(chan struct{})}
			if err = node.handle(nodeEvent{add: peer}); err != nil {
				t.Fatal(err)
			}
			// Warm the path and identity caches before changing Cloud evidence.
			for range 2 {
				if _, _, ok := node.cache.Get(principal, now); !ok {
					t.Fatal("missing warm identity")
				}
			}
			if err = node.revalidatePeerCertificates(now); err != nil {
				t.Fatal(err)
			}
			if link.closed {
				t.Fatal("valid carrier closed")
			}
			// A renewed credential for the same principal must not rescue the old
			// authenticated carrier. Admission remains bound to serial 42.
			replacement := p.Issue(t, 43, principal)
			if _, err = node.cache.Put([][]byte{replacement.Cert.Raw}, now); err != nil {
				t.Fatal(err)
			}
			p.Publish(t, 2, 42)
			revocation.Default.Refresh(context.Background())
			if err = node.revalidatePeerCertificates(time.Now()); err != nil {
				t.Fatal(err)
			}
			if !link.closed || len(node.peers) != 0 || backend.removed != 1 {
				t.Fatalf("revoked link survived: closed=%v peers=%d removed=%d", link.closed, len(node.peers), backend.removed)
			}
			if _, err = ownCreds.Verify([][]byte{remote.Cert.Raw}, time.Now()); !errors.Is(err, revocation.ErrRevoked) {
				t.Fatalf("revoked mesh identity accepted: %v", err)
			}
			if operator {
				if _, err = meshoperator.VerifyPrimary([][]byte{remote.Cert.Raw}, []*x509.Certificate{p.CA}, ownCreds.Device.Tenant(), time.Now()); !errors.Is(err, revocation.ErrRevoked) {
					t.Fatalf("revoked primary operator accepted: %v", err)
				}
			}
			// Revoke the replacement too: cached manifests and route authorizations
			// must disappear even though their signatures have not expired.
			p.Publish(t, 3, 42, 43)
			revocation.Default.Refresh(context.Background())
			node.refresh()
			if _, _, ok := node.cache.Get(principal, time.Now()); ok {
				t.Fatal("cached revoked identity survived")
			}
			for _, m := range node.Snapshot().Devices {
				if m.Device == peerCreds.Device.UUID() {
					t.Fatal("revoked manifest survived")
				}
			}
		})
	}
}
