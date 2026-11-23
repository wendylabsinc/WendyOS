package rtps

import (
	"encoding/binary"
	"testing"
	"time"
)

// A replacement Fast DDS participant is a new reliable discovery reader. It
// needs the retained sequence range even though our participant stayed alive.
func TestSubscriptionHistoryAfterPublisherRestart(t *testing.T) {
	p := wireParticipant(t)
	ep := Endpoint{GUID: GUID{Prefix: GUIDPrefix{8}, EntityID: 0x102}, Topic: "rt/camera", Type: "Image"}
	if err := p.Subscribe(ep); err != nil {
		t.Fatal(err)
	}
	p.Unsubscribe(ep.GUID)
	ep.GUID.Prefix = GUIDPrefix{9}
	if err := p.Subscribe(ep); err != nil {
		t.Fatal(err)
	}
	receiver, loc := listener(t)
	p.peers[ep.GUID.Prefix] = []Locator{loc}
	p.announce()
	_ = receiver.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 65536)
	for {
		n, _, err := receiver.ReadFromUDP(buf)
		if err != nil {
			t.Fatalf("replacement publisher received no reliable subscription history range: %v", err)
		}
		msg, err := ParseMessage(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		for _, sub := range msg.Submessages {
			if sub.Kind != subHEARTBEAT {
				continue
			}
			hb, err := ParseHeartbeat(sub)
			if err != nil {
				t.Fatal(err)
			}
			if hb.WriterID != entitySEDPSubWriter || hb.LastSN != 3 || hb.FirstSN < 1 || hb.FirstSN > 3 {
				t.Fatalf("incorrect retained subscription history: %+v", hb)
			}
			return
		}
	}
}

func TestSubscriptionReplayCoversRetiredHistoryAndBoundsDisposals(t *testing.T) {
	p := wireParticipant(t)
	live := Endpoint{GUID: GUID{Prefix: GUIDPrefix{8}, EntityID: 0x102}, Topic: "rt/stays", Type: "Image"}
	if err := p.Subscribe(live); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		ep := live
		ep.GUID.EntityID = uint32(i+2)<<8 | 2
		if err := p.Subscribe(ep); err != nil {
			t.Fatal(err)
		}
		p.Unsubscribe(ep.GUID)
	}
	if len(p.subHistory) != maxSubscriptionDisposals+1 {
		t.Fatalf("history grew with retired readers: %d", len(p.subHistory))
	}
	if p.subHistory[1] == nil {
		t.Fatal("active old reader was evicted")
	}
	seen := map[SequenceNumber]bool{}
	for _, packet := range p.subscriptionReplayLocked() {
		msg, err := ParseMessage(packet)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range msg.Submessages {
			switch s.Kind {
			case subDATA:
				d, err := ParseData(s)
				if err != nil {
					t.Fatal(err)
				}
				seen[d.WriterSN] = true
			case subGAP:
				g, err := parseGap(s)
				if err != nil {
					t.Fatal(err)
				}
				for n := g.start; n < g.base; n++ {
					seen[n] = true
				}
			}
		}
	}
	for n := SequenceNumber(1); n <= p.seqByWriter[entitySEDPSubWriter]; n++ {
		if !seen[n] {
			t.Fatalf("late reader would wait for missing sequence %d", n)
		}
	}
}

func TestSubscriptionAckReplaysLossAndGapsOnlyRequestedHistory(t *testing.T) {
	p := wireParticipant(t)
	ep := Endpoint{GUID: GUID{Prefix: GUIDPrefix{8}, EntityID: 0x102}, Topic: "rt/camera", Type: "Image"}
	if err := p.Subscribe(ep); err != nil {
		t.Fatal(err)
	}
	p.Unsubscribe(ep.GUID)
	ep.GUID.Prefix = GUIDPrefix{9}
	if err := p.Subscribe(ep); err != nil {
		t.Fatal(err)
	}
	receiver, loc := listener(t)
	p.peers[ep.GUID.Prefix] = []Locator{loc}
	request := buildMessage(ep.GUID.Prefix, buildInfoDst(p.prefix), buildAcknack(entitySEDPSubReader, entitySEDPSubWriter, 1, 3, 1))
	p.handle(request, nil)
	_ = receiver.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 65536)
	var data, gaps int
	for i := 0; i < 4; i++ {
		n, _, err := receiver.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := ParseMessage(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range msg.Submessages {
			if s.Kind == subDATA {
				data++
			}
			if s.Kind == subGAP {
				gaps++
			}
		}
	}
	if data != 2 || gaps != 1 {
		t.Fatalf("replay data=%d gaps=%d", data, gaps)
	}
	// Duplicated requests and final empty ACKs must not create a reply storm.
	p.handle(request, nil)
	p.handle(buildMessage(ep.GUID.Prefix, buildAcknack(entitySEDPSubReader, entitySEDPSubWriter, 4, 3, 2)), nil)
	_ = receiver.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := receiver.ReadFromUDP(buf); err == nil {
		t.Fatal("duplicate/empty ACK generated traffic")
	}
}

func TestSubscriptionAckRejectsMalformedOrForeignRequests(t *testing.T) {
	p := wireParticipant(t)
	peer := GUIDPrefix{9}
	receiver, loc := listener(t)
	p.peers[peer] = []Locator{loc}
	for _, change := range []func([]byte){
		func(b []byte) { binary.LittleEndian.PutUint32(b[16:20], 257) },
		func(b []byte) { binary.LittleEndian.PutUint32(b[8:12], 0xffffffff) },
		func(b []byte) { binary.BigEndian.PutUint32(b[4:8], entitySEDPPubWriter) },
		func(b []byte) { binary.BigEndian.PutUint32(b[:4], entityUnknown) },
	} {
		msg, _ := ParseMessage(buildMessage(peer, buildAcknack(entitySEDPSubReader, entitySEDPSubWriter, 1, 1, 1)))
		s := msg.Submessages[0]
		change(s.Body)
		p.handleSubscriptionAck(peer, s)
	}
	p.handle(buildMessage(peer, buildInfoDst(GUIDPrefix{99}), buildAcknack(entitySEDPSubReader, entitySEDPSubWriter, 1, 1, 1)), nil)
	if len(p.subAckCount) != 0 {
		t.Fatal("invalid/different-destination ACK changed state")
	}
	_ = receiver.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, _, err := receiver.ReadFromUDP(make([]byte, 2048)); err == nil {
		t.Fatal("invalid ACK generated traffic")
	}
}
