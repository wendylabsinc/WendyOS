//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net"
	"net/url"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
)

// Short writes on an unbuffered full-duplex pipe, followed by the real stream
// carrier's one-frame receive queue. Writers cannot progress by an external
// test draining their channels; the opposite protocol reader must consume data.
type duplexSlowConn struct{ net.Conn }

func (c duplexSlowConn) Write(p []byte) (int, error) {
	if len(p) > 64 {
		p = p[:64]
	}
	time.Sleep(100 * time.Microsecond)
	return c.Conn.Write(p)
}

type duplexObservedLink struct {
	*streamNodeLink
	certificateBytes atomic.Int64
	gossipBodies     atomic.Int32
}

func (l *duplexObservedLink) ReadControl() (ControlMessage, error) {
	m, err := l.streamNodeLink.ReadControl()
	if err == nil && m.Certificate != nil && m.Certificate.Chunk != nil {
		l.certificateBytes.Add(int64(len(m.Certificate.Chunk.Data)))
	}
	if err == nil && m.Gossip != nil && m.Gossip.Op == "data" {
		l.gossipBodies.Add(1)
	}
	return m, err
}

func TestDirectoryDuplexSlowFiniteQueuesConverge(t *testing.T) {
	f := newCertificateFixture(t)
	principals := make([]string, 0, len(f.chains))
	for principal := range f.chains {
		principals = append(principals, principal)
	}
	sort.Strings(principals)
	principals = principals[:2]
	keys := make([]*ecdsa.PrivateKey, 2)
	// Keep the existing exact-chain trust fixture, but use real P-256 leaf
	// keys/signatures for the manifests and distinct large parts in each chain.
	for i, principal := range principals {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = key
		device := f.ids[string(f.chains[principal][0])]
		uri, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 1)),
			NotBefore: f.now.Add(-time.Hour), NotAfter: f.now.Add(time.Hour),
			URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature,
			ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 55555, 1}, Value: bytes.Repeat([]byte{byte(40 + i)}, 4096)}}}
		der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
		if err != nil {
			t.Fatal(err)
		}
		chain := cloneChain(f.chains[principal])
		delete(f.ids, string(chain[0]))
		chain[0] = der
		for part := 1; part < len(chain); part++ {
			chain[part] = bytes.Repeat([]byte{byte(60 + i*8 + part)}, len(chain[part]))
		}
		f.chains[principal], f.ids[string(der)] = chain, device
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	left, right := net.Pipe()
	links := []*duplexObservedLink{
		{streamNodeLink: newBoundedStreamNodeLink(duplexSlowConn{left}, 1)},
		{streamNodeLink: newBoundedStreamNodeLink(duplexSlowConn{right}, 1)},
	}
	var workers sync.WaitGroup
	errors := make(chan error, 4)
	t.Cleanup(func() {
		cancel()
		for _, link := range links {
			_ = link.Close()
		}
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("duplex workers did not stop")
		}
	})
	nodes, peers := make([]*Node, 2), make([]*nodePeer, 2)
	wires, have := make([]SignedManifest, 2), make([]ControlMessage, 2)
	chainBytes := make([]int, 2)
	for i, principal := range principals {
		cache := f.cache(t, principal, nil)
		device := f.ids[string(f.chains[principal][0])]
		directory, err := NewDirectory(device.Tenant(), 256, cache)
		if err != nil {
			t.Fatal(err)
		}
		routing, err := NewRouting(babel.Config{RouterID: babel.RouterID(i + 1)}, nil, &routingRecorder{})
		if err != nil {
			t.Fatal(err)
		}
		nodes[i] = &Node{Credentials: &Credentials{Device: device}, cache: cache,
			directory: directory, routing: routing, peers: make(map[babel.LinkID]*nodePeer),
			events: make(chan nodeEvent, 256), ctx: ctx, started: time.Now()}
		peerDevice := f.ids[string(f.chains[principals[1-i]][0])]
		peers[i] = &nodePeer{id: 1, device: peerDevice, link: links[i],
			control: make(chan ControlMessage, 128),
			done:    make(chan struct{}), sync: NewSynchronizer(directory, cache, netgossip.NewPool())}
		nodes[i].peers[1] = peers[i]
		t.Cleanup(peers[i].sync.Close)
		wire, err := SignManifest(routeManifest(device, f.now), f.chains[principal], keys[i], f.now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = directory.Accept(wire, f.now); err != nil {
			t.Fatal(err)
		}
		wires[i] = wire
		messages := peers[i].sync.Record(wire, f.now)
		if len(messages) != 1 || messages[0].Gossip == nil {
			t.Fatal("missing normal HAVE")
		}
		have[i] = messages[0]
		for _, part := range f.chains[principal] {
			chainBytes[i] += len(part)
		}
		if chainBytes[i] < 23<<10 {
			t.Fatal("fixture chain too small", chainBytes[i])
		}
		// Fill both outboxes with an ordinary retained reconciliation burst,
		// leaving the same eight solicited-response credits as production.
		backlog := make([]ControlMessage, cap(peers[i].control)+maxDirectoryReplyFrames-directoryResponseReserve)
		for j := range backlog {
			backlog[j] = have[i]
		}
		if !nodes[i].queueReplies(peers[i], backlog) {
			t.Fatal("normal bulk burst did not fit")
		}
		if len(peers[i].control) != cap(peers[i].control) || len(peers[i].replies) != maxDirectoryReplyFrames-directoryResponseReserve {
			t.Fatal("both output queues were not filled to their bulk credit")
		}
	}
	// Fill each tiny carrier receive queue before the node workers start.
	// No test goroutine subsequently drains these queues or either outbox.
	for i := range links {
		for range 2 {
			if err := links[i].WriteControl(have[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	start := func(f func() error) {
		workers.Add(1)
		go func() { defer workers.Done(); errors <- f() }()
	}
	for i := range nodes {
		n, p := nodes[i], peers[i]
		start(func() error { return n.writeControls(ctx, p) })
		start(func() error { return n.readControls(ctx, p) })
	}
	maintenance := time.NewTicker(5 * time.Millisecond)
	defer maintenance.Stop()
	for {
		select {
		case event := <-nodes[0].events:
			if err := nodes[0].handle(event); err != nil {
				t.Fatal(err)
			}
		case event := <-nodes[1].events:
			if err := nodes[1].handle(event); err != nil {
				t.Fatal(err)
			}
		case <-maintenance.C:
			for i, n := range nodes {
				p := peers[i]
				n.flushReplies(p)
				if err := n.continueIngress(p); err != nil {
					t.Fatal(err)
				}
				replies, changed, err := p.sync.ContinueCertificates(time.Now(), directoryResponseSlots(p))
				if err != nil || len(changed) > 1 {
					t.Fatal("bounded continuation", err, len(changed))
				}
				if !n.queueReplies(p, replies) {
					t.Fatal("duplex response admission failed")
				}
			}
		case err := <-errors:
			t.Fatal("duplex carrier ended before convergence", err)
		case <-ctx.Done():
			t.Fatalf("duplex did not converge: tails=%d/%d ingress=%v/%v bytes=%d/%d",
				len(peers[0].replies), len(peers[1].replies), peers[0].ingress != nil, peers[1].ingress != nil,
				links[0].certificateBytes.Load(), links[1].certificateBytes.Load())
		}
		complete := true
		for i, n := range nodes {
			p := peers[i]
			if len(p.replies) > maxDirectoryReplyFrames || p.replyBytes > maxDirectoryReplyBytes {
				t.Fatal("duplex queue exceeded its existing bound")
			}
			if _, _, ok := n.cache.Get(principals[1-i], time.Now()); !ok {
				complete = false
			}
			found := false
			for _, wire := range n.directory.Records(time.Now()) {
				if wire.Principal == principals[1-i] && bytes.Equal(wire.Body, wires[1-i].Body) && bytes.Equal(wire.Signature, wires[1-i].Signature) {
					found = true
				}
			}
			complete = complete && found
		}
		if complete {
			for i, link := range links {
				if got := link.certificateBytes.Load(); got != int64(chainBytes[1-i]) {
					t.Fatalf("duplex certificate bytes duplicated/lost: side%d got%d want%d", i, got, chainBytes[1-i])
				}
				if link.gossipBodies.Load() != 1 {
					t.Fatalf("side%d manifest DATA count%d", i, link.gossipBodies.Load())
				}
			}
			return
		}
	}
}
