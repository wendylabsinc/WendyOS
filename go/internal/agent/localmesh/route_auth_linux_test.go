//go:build linux

package localmesh

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"net/netip"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/babel"
)

func TestNodeReauthorizesOnSignedManifestChangeAndRestart(t *testing.T) {
	now := time.Now()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	chain := [][]byte{der}
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), func(_ [][]byte, _ time.Time) (Identity, error) {
		return Identity{64, 445, cert.NotAfter}, nil
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(chain, now); err != nil {
		t.Fatal(err)
	}
	directory, err := NewDirectory(64, 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	var receipts []DirectoryReceipt
	if err := directory.SetPersistence(nil, func(next []DirectoryReceipt) error {
		receipts = append([]DirectoryReceipt(nil), next...)
		return nil
	}, now); err != nil {
		t.Fatal(err)
	}
	self, _ := RouterID(64, 460)
	peer, _ := RouterID(64, 445)
	io := &routingRecorder{}
	node := &Node{Credentials: &Credentials{Org: 64, Asset: 460}, directory: directory, cache: cache,
		peers: map[babel.LinkID]*nodePeer{}, started: now, offerSource: func() []int32 { return []int32{445} },
		origins: map[babel.RouterID]bool{}, gateways: map[babel.RouterID]bool{}}
	node.routing, err = NewRouting(babel.Config{RouterID: self, AcceptRoute: func(id babel.RouterID, p netip.Prefix) bool {
		return routeAuthorized(64, node.origins, node.gateways, id, p)
	}}, nil, io)
	if err != nil {
		t.Fatal(err)
	}
	link := &nodePeer{id: 1, control: make(chan ControlMessage, 4), sync: NewSynchronizer(directory, cache)}
	node.peers[1] = link
	manifest := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052, Internet: true}
	accepted, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := node.handle(nodeEvent{id: 1, control: &ControlMessage{Kind: "manifest", Manifest: &accepted}}); err != nil {
		t.Fatal(err)
	}
	if !node.origins[peer] || !node.gateways[peer] || len(io.calls) == 0 {
		t.Fatal("signed manifest did not synchronously refresh Babel admission")
	}
	// Persisted receipts after restart reject replay, but do not create live
	// route authorization while control-plane state is resynchronizing.
	restarted, err := NewDirectory(64, 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.SetPersistence(receipts, func([]DirectoryReceipt) error { return nil }, now); err != nil {
		t.Fatal(err)
	}
	node.directory = restarted
	if err := node.handle(nodeEvent{reauthorize: true}); err != nil {
		t.Fatal(err)
	}
	if node.origins[peer] || node.gateways[peer] {
		t.Fatal("restart receipt incorrectly restored live route authorization")
	}
	// Restore the live directory to isolate signed withdrawal behavior.
	node.directory = directory
	manifest.Revision = 2
	manifest.Withdraw = true
	manifest.Internet = false
	withdrawn, err := SignManifest(manifest, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	before := len(io.calls)
	if err := node.handle(nodeEvent{id: 1, control: &ControlMessage{Kind: "manifest", Manifest: &withdrawn}}); err != nil {
		t.Fatal(err)
	}
	if node.origins[peer] || node.gateways[peer] || len(io.calls) <= before {
		t.Fatal("signed withdrawal did not synchronously revoke and step Babel policy")
	}
}
