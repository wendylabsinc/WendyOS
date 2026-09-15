package rtps

import (
	"context"
	"net"
	"slices"
	"sync"
	"testing"
	"time"
)

// These peers use only loopback unicast sockets: no external DDS graph or
// multicast availability is required to reproduce the discovery feedback.
func newSPDPLoopbackPeer(t *testing.T, id byte) *Participant {
	t.Helper()
	sock, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sock.Close() })
	return &Participant{
		ucast: sock, local: net.IPv4(127, 0, 0, 1), prefix: GUIDPrefix{id},
		peers: map[GUIDPrefix][]Locator{}, seqByWriter: map[uint32]SequenceNumber{},
		seenData: map[uint32]int{},
	}
}

func TestParticipant_SPDPDiscoveryDoesNotAmplifyReplies(t *testing.T) {
	a, b := newSPDPLoopbackPeer(t, 1), newSPDPLoopbackPeer(t, 2)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.readLoop(ctx, a.ucast, false) }()
	go func() { defer wg.Done(); b.readLoop(ctx, b.ucast, false) }()
	defer func() {
		cancel()
		_ = a.Close()
		_ = b.Close()
		wg.Wait()
	}()

	// One initial announcement must complete mutual discovery and then stop.
	// Previously each reply provoked another, producing thousands of packets
	// in this window even when neither participant published application data.
	a.sendTo(b.ucast.LocalAddr().(*net.UDPAddr), a.spdpDatagram())
	deadline := time.Now().Add(time.Second)
	for a.Stats().SPDPAnnounces+b.Stats().SPDPAnnounces < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := a.Stats().SPDPAnnounces + b.Stats().SPDPAnnounces; got != 3 {
		t.Fatalf("one announcement produced %d received SPDP packets; want bounded three-packet discovery", got)
	}
	if a.Stats().PeersSeen != 1 || b.Stats().PeersSeen != 1 {
		t.Fatalf("mutual discovery failed: a=%+v b=%+v", a.Stats(), b.Stats())
	}
}

func TestParticipant_SPDPRepliesWhenPeerLocatorChanges(t *testing.T) {
	p := newSPDPLoopbackPeer(t, 1)
	first, moved := newSPDPLoopbackPeer(t, 2), newSPDPLoopbackPeer(t, 2)
	announce := func(remote *Participant) []Locator {
		t.Helper()
		locator := udpv4Locator(remote.local, remote.ucast.LocalAddr().(*net.UDPAddr).Port)
		b := newPLBuilder()
		b.addLocator(pidMetatrafficUnicastLocator, locator)
		p.handleSPDP(remote.prefix, &DataSubmessage{Payload: b.finish()})
		return []Locator{locator}
	}
	readReply := func(remote *Participant, want bool) {
		t.Helper()
		wait := 200 * time.Millisecond
		if !want {
			wait = 30 * time.Millisecond
		}
		if err := remote.ucast.SetReadDeadline(time.Now().Add(wait)); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, 4096)
		n, _, err := remote.ucast.ReadFromUDP(buf)
		if !want {
			if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
				t.Fatalf("unchanged peer received another reply: bytes=%d err=%v", n, err)
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		msg, err := ParseMessage(buf[:n])
		if err != nil || msg.Prefix != p.prefix || len(msg.Submessages) != 1 {
			t.Fatalf("invalid SPDP reply: message=%+v err=%v", msg, err)
		}
		data, err := ParseData(msg.Submessages[0])
		if err != nil || data.WriterID != entitySPDPWriter {
			t.Fatalf("reply was not a participant announcement: data=%+v err=%v", data, err)
		}
	}

	announce(first)
	readReply(first, true)
	announce(first)
	readReply(first, false)
	wantLocators := announce(moved)
	readReply(moved, true)
	announce(moved)
	readReply(moved, false)
	if !slices.Equal(p.peers[first.prefix], wantLocators) || p.Stats().PeersSeen != 1 {
		t.Fatalf("locator refresh changed peer identity or retained stale endpoint: peers=%v stats=%+v", p.peers, p.Stats())
	}
}
