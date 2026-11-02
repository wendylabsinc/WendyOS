//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
)

type ingressFixtureLink struct {
	input  chan ControlMessage
	reads  atomic.Int32
	closed atomic.Bool
}

func (l *ingressFixtureLink) Close() error                                  { l.closed.Store(true); return nil }
func (*ingressFixtureLink) SendDatagram([]byte) error                       { return nil }
func (*ingressFixtureLink) ReceiveDatagram(context.Context) ([]byte, error) { return nil, io.EOF }
func (*ingressFixtureLink) WriteControl(ControlMessage) error               { return nil }
func (l *ingressFixtureLink) ReadControl() (ControlMessage, error) {
	l.reads.Add(1)
	m, ok := <-l.input
	if !ok {
		return m, io.EOF
	}
	return m, nil
}

func TestDirectoryResponseBatchUsesFreeChannelPrefix(t *testing.T) {
	n := &Node{}
	p := &nodePeer{control: make(chan ControlMessage, 128)}
	messages := make([]ControlMessage, 96)
	for i := range messages {
		messages[i] = ControlMessage{Kind: "identity-request", Principal: testPrincipal(testTenantUUID, testDeviceAID)}
	}
	if !n.queueReplies(p, messages) || len(p.control) != 96 || len(p.replies) != 0 || p.replyBytes != 0 {
		t.Fatal("batch fitting free channel slots was rejected as a32-frame reserve overflow")
	}
	// Large supported legacy bundles are charged at their actual encoded size,
	// not the2KiB credit unit used by chunked response producers.
	f := newCertificateFixture(t)
	p = &nodePeer{control: make(chan ControlMessage, 2)}
	messages = nil
	for _, chain := range f.chains {
		messages = append(messages, ControlMessage{Kind: "bundle", Bundle: chain})
	}
	if !n.queueReplies(p, messages) || len(p.control) != 2 || len(p.replies) != 1 || p.replyBytes > maxDirectoryReplyBytes {
		t.Fatal("large bundle prefix was spuriously charged to the retained tail")
	}
}

func TestDirectoryKnownResponsesFitBinaryCreditUnit(t *testing.T) {
	principal := testPrincipal(testTenantUUID, testDeviceAID)
	chain := make([][]byte, 16)
	for i := range chain {
		chain[i] = bytes.Repeat([]byte{byte(i + 1)}, 1024)
	}
	hash := certificateHash(chain[0])
	frames := []ControlMessage{
		{Kind: "certificate", Certificate: DescribeCertificates(principal, chain)},
		{Kind: "certificate", Certificate: &CertificateMessage{Chunk: &CertificateChunk{Hash: hash, Offset: 63 << 10, Total: 64 << 10, Data: chain[0]}}},
		{Kind: "identity-request", Principal: principal},
		{Kind: "gossip", Gossip: &netgossip.Message{Version: 1, Type: netgossip.Manifest, Op: "data", Digest: make([]byte, 32), Request: 1<<63 + 17, Offset: 63 << 10, Total: 64 << 10, Data: chain[0]}},
		{Kind: "time", Time: &TimestampControl{ID: 1<<63 + 17, Op: "reply", Offset: 63 << 10, Total: 64 << 10, Data: chain[0]}},
	}
	for _, frame := range frames {
		wire, err := netwire.Marshal(frame)
		if err != nil || len(wire) > directoryResponseBytes {
			t.Fatalf("%s exceeds binary response credit: bytes=%d error=%v", frame.Kind, len(wire), err)
		}
	}
}

func TestDirectorySlowReaderRetainsCertificateRequestsAndOtherPeerProgress(t *testing.T) {
	f := newCertificateFixture(t)
	var principals []string
	for principal := range f.chains {
		principals = append(principals, principal)
	}
	sourceCache := f.cache(t, principals[0], nil)
	receiverCache := f.cache(t, "", nil)
	local := f.ids[string(f.chains[principals[0]][0])]
	remote := f.ids[string(f.chains[principals[1]][0])]
	directory, err := NewDirectory(local.Tenant(), 1, sourceCache)
	if err != nil {
		t.Fatal(err)
	}
	routing, err := NewRouting(babel.Config{RouterID: 1}, nil, &routingRecorder{})
	if err != nil {
		t.Fatal(err)
	}
	n := &Node{Credentials: &Credentials{Device: local}, cache: sourceCache, directory: directory, routing: routing, peers: make(map[babel.LinkID]*nodePeer), started: time.Now()}
	link := &ingressFixtureLink{}
	p := &nodePeer{id: 1, device: remote, link: link, control: make(chan ControlMessage, 128), done: make(chan struct{}), sync: NewSynchronizer(directory, sourceCache, netgossip.NewPool())}
	other := &nodePeer{id: 2, device: remote, link: &ingressFixtureLink{}, control: make(chan ControlMessage, 128), done: make(chan struct{}), sync: NewSynchronizer(directory, sourceCache, netgossip.NewPool())}
	n.peers[p.id] = p
	n.peers[other.id] = other
	defer p.sync.Close()
	defer other.sync.Close()
	// A prior ordinary burst is still admitted while the peer starts draining.
	bulk := ControlMessage{Kind: "gossip", Gossip: &netgossip.Message{Version: 1, Type: netgossip.Manifest, Op: "have", Digest: make([]byte, 32)}}
	for range cap(p.control) {
		p.control <- bulk
	}
	for range maxDirectoryReplyFrames {
		if !n.queueReplies(p, []ControlMessage{bulk}) {
			t.Fatal("initial backlog did not fit existing bounds")
		}
	}
	receiver := NewCertificateExchange(receiverCache, local.Tenant())
	defer receiver.Close()
	requests, _, err := receiver.Receive(*DescribeCertificates(principals[0], f.chains[principals[0]]), f.now)
	if err != nil || len(requests) != 1 {
		t.Fatal("missing initial certificate request", err)
	}
	ack := make(chan error, 1)
	first := ControlMessage{Kind: "certificate", Certificate: requests[0]}
	if err = n.handle(nodeEvent{id: p.id, control: &first, controlResult: ack}); err != nil {
		t.Fatal(err)
	}
	if p.ingress == nil {
		t.Fatal("full output did not defer its one incoming request")
	}
	select {
	case <-ack:
		t.Fatal("reader was acknowledged before its request could be handled")
	default:
	}
	// The other peer's actor work is independent of this blocked incarnation.
	secondAck := make(chan error, 1)
	query := ControlMessage{Kind: "identity-request", Principal: principals[0]}
	if err = n.handle(nodeEvent{id: other.id, control: &query, controlResult: secondAck}); err != nil {
		t.Fatal(err)
	}
	if err = <-secondAck; err != nil || len(other.control) != 1 {
		t.Fatal("another peer was stalled", err)
	}

	completed := false
	received := 0
	for steps := 0; steps < 1024 && !completed; steps++ {
		var message ControlMessage
		select {
		case message = <-p.control:
		default:
		}
		n.flushReplies(p) // The production maintenance tick drains retained replies too.
		if err = n.continueIngress(p); err != nil {
			t.Fatal(err)
		}
		if p.ingress == nil {
			select {
			case err = <-ack:
				if err != nil {
					t.Fatal(err)
				}
			default:
			}
		}
		if len(p.replies) > maxDirectoryReplyFrames || p.replyBytes > maxDirectoryReplyBytes || link.closed.Load() {
			t.Fatal("ordinary slow reader exceeded bounds or closed carrier")
		}
		if message.Certificate == nil || message.Certificate.Chunk == nil {
			continue
		}
		// Cross real framing/CBOR decoding before receiver-side assembly.
		var wire bytes.Buffer
		if err = WriteControl(&wire, message); err != nil {
			t.Fatal(err)
		}
		message, err = ReadControl(&wire)
		if err != nil {
			t.Fatal(err)
		}
		received += len(message.Certificate.Chunk.Data)
		requests, done, e := receiver.Receive(*message.Certificate, f.now)
		if e != nil {
			t.Fatal(e)
		}
		for _, principal := range done {
			if principal == principals[0] {
				completed = true
			}
		}
		if len(requests) > 1 {
			t.Fatal("certificate download exceeded one in-flight request")
		}
		if len(requests) == 1 {
			next := ControlMessage{Kind: "certificate", Certificate: requests[0]}
			if err = n.handle(nodeEvent{id: p.id, control: &next, controlResult: ack}); err != nil {
				t.Fatal(err)
			}
		}
	}
	want := 0
	for _, part := range f.chains[principals[0]] {
		want += len(part)
	}
	if !completed || received != want || want < 23<<10 {
		t.Fatalf("ordinary23KiB chain did not converge exactly once: completed=%v received=%d want=%d", completed, received, want)
	}
	if _, _, ok := receiverCache.Get(principals[0], f.now); !ok {
		t.Fatal("completed certificate identity missing")
	}
}

func TestDirectoryIngressAcknowledgementCancellation(t *testing.T) {
	input := make(chan ControlMessage, 2)
	input <- ControlMessage{Kind: "future-note"}
	input <- ControlMessage{Kind: "future-note"}
	link := &ingressFixtureLink{input: input}
	p := &nodePeer{id: 1, link: link, done: make(chan struct{})}
	n := &Node{events: make(chan nodeEvent, 4)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.readControls(ctx, p) }()
	select {
	case <-n.events:
	case <-time.After(time.Second):
		t.Fatal("reader did not send first input")
	}
	if got := link.reads.Load(); got != 1 {
		t.Fatalf("reader advanced without actor acknowledgement: %d", got)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("peer cancellation waited for node shutdown")
	}
}
