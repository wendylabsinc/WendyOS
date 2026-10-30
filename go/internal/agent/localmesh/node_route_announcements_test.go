package localmesh

import (
	"crypto"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

type routePublicationSigner struct {
	crypto.Signer
	beforeSign func()
}

func (s *routePublicationSigner) Sign(random io.Reader, digest []byte, opts crypto.SignerOpts) ([]byte, error) {
	s.beforeSign()
	return s.Signer.Sign(random, digest, opts)
}

// Construct only the publication dependencies. Calling Persist does not need a
// kernel or a live link, and refresh can read the real Routing engine's snapshot.
func routePublicationNode(t *testing.T) *Node {
	t.Helper()
	credentials, _ := testCredentials(t)
	now := time.Now()
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), credentials.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Put(credentials.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	directory, err := NewDirectory(credentials.Device.Tenant(), 10, cache)
	if err != nil {
		t.Fatal(err)
	}
	store, state, err := OpenStateStore(filepath.Join(t.TempDir(), "state.json"), credentials.Device, 1)
	if err != nil {
		t.Fatal(err)
	}
	routing, err := NewRouting(babel.Config{RouterID: 1}, nil, &routingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{
		Credentials: credentials, cache: cache, directory: directory, store: store,
		state: state, routing: routing, peers: make(map[babel.LinkID]*nodePeer), started: now,
		advertisement: Manifest{Version: ManifestVersionRoute, Device: credentials.Device.UUID(), AgentPort: 50052},
	}
	if err := directory.SetPersistence(nil, func(receipts []DirectoryReceipt) error {
		next := n.state
		next.Receipts = receipts
		if err := n.store.Save(next); err != nil {
			return err
		}
		n.state = next
		return nil
	}, now); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNodeRoutePublicationPersistsBeforeSigning(t *testing.T) {
	n := routePublicationNode(t)
	checkpoint := n.routing.Checkpoint()
	wantRevision := uint64(1)
	signatures := 0
	n.Credentials.Signer = &routePublicationSigner{Signer: n.Credentials.Signer, beforeSign: func() {
		signatures++
		// Read the real durable file at the point the private key is used.
		persisted, _, err := readPersistentState(n.store.path)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Babel == nil || persisted.Babel.Seqno != checkpoint.Seqno || persisted.Revision != wantRevision {
			t.Fatalf("signed before checkpoint/revision persistence: %+v", persisted)
		}
	}}
	if err := n.Persist(checkpoint); err != nil {
		t.Fatal(err)
	}
	if signatures != 1 {
		t.Fatalf("initial checkpoint signed %d announcements", signatures)
	}
	manifest := n.advertisement
	if manifest.Version != ManifestVersionRoute || manifest.Route == nil || manifest.Route.Seqno != checkpoint.Seqno || manifest.Route.RouterID != checkpoint.RouterID || manifest.Route.Prefix != netip.PrefixFrom(n.Credentials.Device.MeshAddress(), 128) {
		t.Fatalf("initial checkpoint produced incorrect route proof: %+v", manifest)
	}
	if manifest.Revision != 1 || manifest.Expires <= manifest.Issued || manifest.Expires-manifest.Issued > MaxLease.Milliseconds() {
		t.Fatalf("invalid publication lease: %+v", manifest)
	}
	// Another Directory must independently accept the emitted signature.
	verifier, err := NewDirectory(n.Credentials.Device.Tenant(), 10, n.cache)
	if err != nil {
		t.Fatal(err)
	}
	records := n.directory.Records(time.Now())
	if len(records) != 1 {
		t.Fatalf("expected one published proof, got %d", len(records))
	}
	if changed, err := verifier.Accept(records[0], time.Now()); err != nil || !changed {
		t.Fatalf("published signature failed verification: changed=%v err=%v", changed, err)
	}

	// Persisting unrelated Babel state must not consume an origin revision or
	// shorten the currently advertised lease by publishing another manifest.
	checkpoint.LastLink++
	if err := n.Persist(checkpoint); err != nil {
		t.Fatal(err)
	}
	if signatures != 1 || n.state.Revision != 1 || !reflect.DeepEqual(n.advertisement, manifest) {
		t.Fatal("unchanged sequence churned the signed announcement")
	}
	checkpoint.Seqno++
	// Model an idle interval so this change publishes immediately.
	n.nextPublication = time.Now().Add(-time.Millisecond)
	wantRevision++
	if err := n.Persist(checkpoint); err != nil {
		t.Fatal(err)
	}
	if signatures != 2 || n.advertisement.Revision != wantRevision || n.advertisement.Route.Seqno != checkpoint.Seqno {
		t.Fatalf("new checkpoint sequence was not published: %+v", n.advertisement)
	}
}

func TestNodeRoutePublicationFailureDoesNotExposeNewProof(t *testing.T) {
	for _, phase := range []string{"checkpoint", "origin revision"} {
		t.Run(phase, func(t *testing.T) {
			n := routePublicationNode(t)
			checkpoint := n.routing.Checkpoint()
			if err := n.Persist(checkpoint); err != nil {
				t.Fatal(err)
			}
			before := n.advertisement
			records := n.directory.Records(time.Now())
			originalPath := n.store.path
			blocker := filepath.Join(t.TempDir(), "not-a-directory")
			if err := os.WriteFile(blocker, []byte("block writes"), 0600); err != nil {
				t.Fatal(err)
			}
			brokenPath := filepath.Join(blocker, "state.json")
			n.Credentials.Signer = &routePublicationSigner{Signer: n.Credentials.Signer, beforeSign: func() {
				t.Fatal("private key used despite failed persistence")
			}}
			checkpoint.Seqno++
			var publishErr error
			if phase == "checkpoint" {
				n.store.path = brokenPath
				publishErr = n.Persist(checkpoint)
			} else {
				// Persist the new checkpoint inside the signing gate, then fail
				// the actual origin write when publication becomes eligible.
				// Cached identities need not invoke the verifier on publication.
				if err := n.Persist(checkpoint); err != nil {
					t.Fatal(err)
				}
				n.store.path = brokenPath
				n.nextPublication = time.Now().Add(-time.Millisecond)
				publishErr = n.publish()
			}
			if publishErr == nil {
				t.Fatal("persistence failure ignored")
			}
			if !reflect.DeepEqual(n.advertisement, before) || !reflect.DeepEqual(n.directory.Records(time.Now()), records) {
				t.Fatal("failed persistence exposed a new proof")
			}
			persisted, _, err := readPersistentState(originalPath)
			if err != nil {
				t.Fatal(err)
			}
			wantSequence := before.Route.Seqno
			if phase == "origin revision" {
				wantSequence = checkpoint.Seqno
			}
			if persisted.Babel.Seqno != wantSequence || persisted.Revision != before.Revision {
				t.Fatalf("unexpected durable state after failed %s write: %+v", phase, persisted)
			}
		})
	}
}

func TestNodeRoutePublicationSnapshotOwnsRouteBinding(t *testing.T) {
	n := routePublicationNode(t)
	if err := n.Persist(n.routing.Checkpoint()); err != nil {
		t.Fatal(err)
	}
	snapshot := n.Snapshot()
	if len(snapshot.Devices) != 1 || snapshot.Devices[0].Route == nil {
		t.Fatal("published proof missing from node snapshot")
	}
	want := *snapshot.Devices[0].Route
	if snapshot.Self != n.Credentials.Device || snapshot.CapturedAt.IsZero() || !snapshot.RouteAuthorizations[want] {
		t.Fatal("snapshot lacks its enrolled identity, capture time, or admitted route origin")
	}
	delete(snapshot.RouteAuthorizations, want)
	if !n.Snapshot().RouteAuthorizations[want] {
		t.Fatal("caller mutated node route authorization through snapshot")
	}
	snapshot.Devices[0].Route.Seqno++
	snapshot.Devices[0].Route.RouterID++
	if got := *n.Snapshot().Devices[0].Route; got != want {
		t.Fatalf("caller mutated node's route proof through snapshot: %+v", got)
	}
	if got := *n.directory.Snapshot(time.Now())[0].Route; got != want {
		t.Fatalf("caller mutated Directory's route proof through snapshot: %+v", got)
	}
}

func TestNodeRefreshVerifiesEachIdentityOnce(t *testing.T) {
	n := routePublicationNode(t)
	if err := n.Persist(n.routing.Checkpoint()); err != nil {
		t.Fatal(err)
	}
	_, peer := testCredentials(t)
	now := time.Now()
	if _, err := n.cache.Put(peer.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	m := routeManifest(peer.Device, now)
	w, err := SignManifest(m, peer.Certificate.Certificate, peer.Signer, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := n.directory.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	// Count actual certificate-chain validations, not a fake verifier that
	// merely returns a trusted principal. Both records use the real test CA.
	verify := n.cache.verify
	checks := 0
	n.cache.verify = func(chain [][]byte, at time.Time) (Identity, error) {
		checks++
		return verify(chain, at)
	}
	n.refresh()
	snapshot := n.Snapshot()
	if checks != 2 || len(snapshot.Devices) != 2 || len(snapshot.RouteAuthorizations) != 2 {
		t.Fatalf("refresh checked %d chains for %d devices and %d route origins", checks, len(snapshot.Devices), len(snapshot.RouteAuthorizations))
	}
	if !snapshot.RouteAuthorizations[*m.Route] || !snapshot.RouteAuthorizations[*n.advertisement.Route] {
		t.Fatal("refresh did not derive both signed origins from its verified manifests")
	}
}

type sequencePublicationIO struct {
	*routingRecorder
	node  *Node
	check func(babel.Datagram)
}

func (r *sequencePublicationIO) Persist(cp babel.Checkpoint) error { return r.node.Persist(cp) }
func (r *sequencePublicationIO) Send(d babel.Datagram) error {
	if r.check != nil {
		r.check(d)
	}
	return r.routingRecorder.Send(d)
}

func TestNodeSequenceFanoutPersistsAndQueuesGossipProofBeforeSend(t *testing.T) {
	n := routePublicationNode(t)
	// Router neighbours share one announcement pool and fetch exact signed
	// artifacts through HAVE/WANT rather than an independent service stream.
	pool := netgossip.NewPool()
	io := &sequencePublicationIO{routingRecorder: &routingRecorder{}, node: n}
	var err error
	n.routing, err = NewRouting(babel.Config{RouterID: 1, HelloInterval: time.Second, UpdateInterval: 4 * time.Second}, nil, io)
	if err != nil {
		t.Fatal(err)
	}
	prefix := netip.PrefixFrom(n.Credentials.Device.MeshAddress(), 128)
	for _, id := range []babel.LinkID{1, 2} {
		n.peers[id] = &nodePeer{id: id, control: make(chan ControlMessage, 16), sync: NewSynchronizer(n.directory, n.cache, pool)}
		defer n.peers[id].sync.Close()
		link := babel.Link{ID: id, Local: netip.MustParseAddr("fe80::1"), Peer: netip.MustParseAddr("fe80::2"), Cost: 64}
		if err := n.routing.Step(0, babel.AddLink{Link: link}); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.routing.Step(0, babel.Originate{Prefix: prefix}); err != nil {
		t.Fatal(err)
	}
	for now := 100 * time.Millisecond; now <= 500*time.Millisecond; now += 100 * time.Millisecond {
		if err := n.routing.Step(now, babel.Tick{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, peer := range n.peers {
		for len(peer.control) != 0 {
			<-peer.control
		}
	}
	checked := make(map[babel.LinkID]bool)
	io.check = func(d babel.Datagram) {
		if checked[d.Link] {
			return
		}
		persisted, _, err := readPersistentState(n.store.path)
		if err != nil {
			t.Fatal(err)
		}
		if persisted.Babel == nil || persisted.Babel.Seqno != 1 || persisted.Revision != n.advertisement.Revision {
			t.Fatalf("link %d sent before sequence/revision persistence: %+v", d.Link, persisted)
		}
		// Persist must queue the new proof's HAVE before releasing a Babel
		// datagram on either link. Download that exact artifact through the pooled
		// protocol and verify it independently before checking its durable receipt.
		receivedDirectory, err := NewDirectory(n.Credentials.Device.Tenant(), 10, n.cache)
		if err != nil {
			t.Fatal(err)
		}
		receiver := NewSynchronizer(receivedDirectory, n.cache, netgossip.NewPool())
		defer receiver.Close()
		var proof *SignedManifest
		for len(n.peers[d.Link].control) != 0 {
			message := <-n.peers[d.Link].control
			if message.Gossip == nil || message.Gossip.Op != "have" {
				t.Fatal("router publication bypassed HAVE/WANT", message.Kind)
			}
			changed := exchangeDirectoryMessages(t, n.peers[d.Link].sync, receiver, []ControlMessage{message}, time.Now())
			if len(changed) != 1 {
				t.Fatal("queued HAVE did not provide one verified proof", len(changed))
			}
			proof = &changed[0]
		}
		if proof == nil {
			t.Fatalf("link %d sent before its new proof was queued", d.Link)
		}
		var manifest Manifest
		if err := netwire.Unmarshal(proof.Body, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.Route == nil || manifest.Route.Seqno != 1 || manifest.Revision != persisted.Revision {
			t.Fatalf("link %d queued the wrong proof: %+v", d.Link, manifest)
		}
		if len(persisted.Receipts) != 1 || persisted.Receipts[0].Revision != manifest.Revision || persisted.Receipts[0].Hash != manifestHash(proof.Body) {
			t.Fatalf("link %d sent before its signed proof receipt was durable", d.Link)
		}
		verifier, err := NewDirectory(n.Credentials.Device.Tenant(), 10, n.cache)
		if err != nil {
			t.Fatal(err)
		}
		if changed, err := verifier.Accept(*proof, time.Now()); err != nil || !changed {
			t.Fatalf("link %d proof failed independent signature verification: %v", d.Link, err)
		}
		checked[d.Link] = true
	}
	// Model an idle publication gate before this sequence change.
	n.nextPublication = time.Now().Add(-time.Millisecond)
	if err := n.routing.Step(500*time.Millisecond, babel.Receive{ID: 1, Packet: routeSequenceRequest(prefix, 1, 1)}); err != nil {
		t.Fatal(err)
	}
	if err := n.routing.Step(600*time.Millisecond, babel.Tick{}); err != nil {
		t.Fatal(err)
	}
	if !checked[1] || !checked[2] {
		t.Fatalf("sequence fanout did not exercise both persistence barriers: %v", checked)
	}
}

// A service edit starts the one-second signing gate immediately before a real
// local sequence request. Keep forwarding and ordinary control/liveness active,
// then publish/fetch the signed proof before promptly exporting the new sequence.
func TestNodeSequenceDuringServicePublicationGate(t *testing.T) {
	n := routePublicationNode(t)
	io := &sequencePublicationIO{routingRecorder: &routingRecorder{}, node: n}
	var err error
	n.routing, err = NewRouting(babel.Config{RouterID: 1, HelloInterval: time.Second, UpdateInterval: 16 * time.Second, AdvertiseLocalRoute: n.advertiseLocalRoute}, nil, io)
	if err != nil {
		t.Fatal(err)
	}
	prefix := netip.PrefixFrom(n.Credentials.Device.MeshAddress(), 128)
	elapsed := time.Duration(0)
	seen := make(map[babel.LinkID]bool)
	receivers := make(map[babel.LinkID]*Routing)
	directories := make(map[babel.LinkID]*Directory)
	pool := netgossip.NewPool()
	for _, id := range []babel.LinkID{1, 2} {
		n.peers[id] = &nodePeer{id: id, control: make(chan ControlMessage, 128), sync: NewSynchronizer(n.directory, n.cache, pool)}
		defer n.peers[id].sync.Close()
		directory, err := NewDirectory(n.Credentials.Device.Tenant(), 10, n.cache)
		if err != nil {
			t.Fatal(err)
		}
		directories[id] = directory
		receiver, err := NewRouting(babel.Config{RouterID: babel.RouterID(10 + id), AcceptRouteUpdate: func(origin babel.RouterID, p netip.Prefix, seq uint16) bool {
			allowed := directory.RouteAuthorizations(time.Now())[RouteOrigin{RouterID: origin, Prefix: p, Seqno: seq}]
			if origin == 1 && p == prefix && seq == 1 {
				if !allowed {
					t.Fatal("new local sequence reached Babel before its signed proof")
				}
				seen[id] = true
			}
			return allowed
		}}, nil, &routingRecorder{})
		if err != nil {
			t.Fatal(err)
		}
		receivers[id] = receiver
		if err := receiver.Step(0, babel.AddLink{Link: babel.Link{ID: id, Local: netip.MustParseAddr("fe80::2"), Peer: netip.MustParseAddr("fe80::1"), Cost: 64}}); err != nil {
			t.Fatal(err)
		}
		if err := n.routing.Step(0, babel.AddLink{Link: babel.Link{ID: id, Local: netip.MustParseAddr("fe80::1"), Peer: netip.MustParseAddr("fe80::2"), Cost: 64}}); err != nil {
			t.Fatal(err)
		}
	}
	io.check = func(d babel.Datagram) {
		// Fetch and independently verify the queued exact signed artifact before
		// delivering the accompanying Babel output to its normal route consumer.
		receiver := NewSynchronizer(directories[d.Link], n.cache, netgossip.NewPool())
		defer receiver.Close()
		for len(n.peers[d.Link].control) > 0 {
			message := <-n.peers[d.Link].control
			exchangeDirectoryMessages(t, n.peers[d.Link].sync, receiver, []ControlMessage{message}, time.Now())
		}
		if err := receivers[d.Link].Step(elapsed, babel.Receive{ID: d.Link, Packet: d.Payload}); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.routing.Step(0, babel.Originate{Prefix: prefix}); err != nil {
		t.Fatal(err)
	}
	for elapsed = 100 * time.Millisecond; elapsed <= 500*time.Millisecond; elapsed += 100 * time.Millisecond {
		if err := n.routing.Step(elapsed, babel.Tick{}); err != nil {
			t.Fatal(err)
		}
	}
	// Model an idle interval: the first service edit must sign immediately.
	n.nextPublication = time.Now().Add(-time.Millisecond)
	before := n.state.Revision
	services := []ManifestService{{Mesh: "default", AppID: "com.wendy.test", ServiceID: "temperature", Type: "_sensorlink._tcp", Instance: "Room", Port: 28080}}
	if err := n.handle(nodeEvent{services: &services}); err != nil || n.state.Revision != before+1 {
		t.Fatal("idle service edit did not publish immediately", err)
	}
	serviceRevision := n.state.Revision
	elapsed = 600 * time.Millisecond
	if err := n.routing.Step(elapsed, babel.Receive{ID: 1, Packet: routeSequenceRequest(prefix, 1, 1)}); err != nil {
		t.Fatal(err)
	}
	livenessBefore := len(io.sent)
	for elapsed = 700 * time.Millisecond; elapsed <= 1200*time.Millisecond; elapsed += 100 * time.Millisecond {
		if err := n.routing.Step(elapsed, babel.Tick{}); err != nil {
			t.Fatal(err)
		}
	}
	if seen[1] || seen[2] || n.state.Revision != serviceRevision || !n.publicationDirty {
		t.Fatal("rate gate exported or signed a new origin prematurely")
	}
	if len(io.sent) <= livenessBefore {
		t.Fatal("publication gate stopped ordinary Babel liveness output")
	}
	if err := n.handleControl(n.peers[1], &ControlMessage{Kind: "future-optional-control"}); err != nil {
		t.Fatal("publication gate blocked control processing", err)
	}
	local := n.routing.Snapshot().Routes
	if len(local) != 1 || !local[0].Local || local[0].Unreachable || local[0].Seqno != 1 {
		t.Fatal("publication gate prevented the forwarding transaction from committing", local)
	}
	// Model the publication deadline without sleeping or changing signed time.
	n.nextPublication = time.Now().Add(-time.Millisecond)
	if err := n.publish(); err != nil || n.state.Revision != serviceRevision+1 {
		t.Fatal("coalesced origin did not publish at the gate", err)
	}
	deadline, ok := n.routing.NextDeadline()
	if !ok || deadline > n.routing.Snapshot().Now+150*time.Millisecond {
		t.Fatal("pending origin waits for a periodic update instead of prompt retry", deadline)
	}
	elapsed = deadline
	if err := n.routing.Step(elapsed, babel.Tick{}); err != nil {
		t.Fatal(err)
	}
	if !seen[1] || !seen[2] || n.state.Revision != serviceRevision+1 || len(n.advertisement.Services) != 1 {
		t.Fatal("published sequence did not promptly reach both neighbours without another signature", seen)
	}
}
