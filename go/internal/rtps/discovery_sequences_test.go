package rtps

import (
	"encoding/binary"
	"net"
	"reflect"
	"testing"
	"time"
)

func discoveryHeartbeat(first, last SequenceNumber) Submessage {
	b := make([]byte, 28)
	binary.BigEndian.PutUint32(b[4:8], entitySEDPPubWriter)
	put := func(off int, sn SequenceNumber) {
		binary.LittleEndian.PutUint32(b[off:off+4], uint32(sn>>32))
		binary.LittleEndian.PutUint32(b[off+4:off+8], uint32(sn))
	}
	put(8, first)
	put(16, last)
	return Submessage{Kind: subHEARTBEAT, Endian: binary.LittleEndian, Body: b}
}

func requestedDiscovery(t *testing.T, receiver *net.UDPConn) []SequenceNumber {
	t.Helper()
	receiver.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 2048)
	n, _, err := receiver.ReadFromUDP(b)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := ParseMessage(b[:n])
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range msg.Submessages {
		if s.Kind != subACKNACK {
			continue
		}
		base := SequenceNumber(int64(int32(s.Endian.Uint32(s.Body[8:12])))<<32 | int64(s.Endian.Uint32(s.Body[12:16])))
		bits := s.Endian.Uint32(s.Body[16:20])
		var requested []SequenceNumber
		for i := uint32(0); i < bits; i++ {
			if s.Endian.Uint32(s.Body[20+4*(i/32):24+4*(i/32)])&(1<<(31-i%32)) != 0 {
				requested = append(requested, base+SequenceNumber(i))
			}
		}
		return requested
	}
	t.Fatal("no ACKNACK")
	return nil
}

func assertDiscoverySilent(t *testing.T, p *Participant, peer GUIDPrefix, hb Submessage) {
	t.Helper()
	before := p.Stats().AcksSent
	for range 20 {
		p.handleHeartbeat(peer, hb, nil)
	}
	if p.Stats().AcksSent != before {
		t.Fatal("complete discovery re-requested known history")
	}
}

func TestDiscoveryRecoversEarlierAnnouncements(t *testing.T) {
	p := wireParticipant(t)
	receiver, loc := listener(t)
	peer := GUIDPrefix{1, 15, 99}
	p.peers[peer] = []Locator{loc}
	receive := func(sn SequenceNumber) {
		ep := Endpoint{GUID: GUID{Prefix: peer, EntityID: uint32(sn)<<8 | 0x02}, Topic: "rt/thermal", Type: "sensor_msgs::msg::dds_::Image_"}
		d := publicationData(ep, sn)
		p.handle(buildMessage(peer, buildData(entitySEDPPubReader, entitySEDPPubWriter, sn, d.Payload)), nil)
	}
	receive(10)
	hb := discoveryHeartbeat(1, 10)
	p.handleHeartbeat(peer, hb, nil)
	if got := requestedDiscovery(t, receiver); !reflect.DeepEqual(got, []SequenceNumber{1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("missing history not requested: %v", got)
	}
	for _, sn := range []SequenceNumber{8, 3, 1, 9, 2, 7, 4, 6, 5} {
		receive(sn)
	}
	if len(p.Endpoints()) != 10 {
		t.Fatalf("only %d endpoints recovered", len(p.Endpoints()))
	}
	assertDiscoverySilent(t, p, peer, hb)
	p.mu.Lock()
	p.removePeerLocked(peer)
	p.mu.Unlock()
	p.handleHeartbeat(peer, hb, receiver.LocalAddr().(*net.UDPAddr))
	if len(requestedDiscovery(t, receiver)) != 10 {
		t.Fatal("expired peer retained acknowledgement history")
	}
}

func discoveryGap(order binary.ByteOrder, start, base SequenceNumber, bits uint32, bitmap ...uint32) Submessage {
	b := make([]byte, 28+4*len(bitmap))
	binary.BigEndian.PutUint32(b[:4], entitySEDPPubReader)
	binary.BigEndian.PutUint32(b[4:8], entitySEDPPubWriter)
	put := func(off int, sn SequenceNumber) {
		order.PutUint32(b[off:off+4], uint32(sn>>32))
		order.PutUint32(b[off+4:off+8], uint32(sn))
	}
	put(8, start)
	put(16, base)
	order.PutUint32(b[24:28], bits)
	for i, v := range bitmap {
		order.PutUint32(b[28+4*i:32+4*i], v)
	}
	return Submessage{Kind: subGAP, Endian: order, Body: b}
}

func TestDiscoveryGapAndSparseReplay(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		p := wireParticipant(t)
		receiver, loc := listener(t)
		peer := GUIDPrefix{1, 15, 98}
		p.peers[peer] = []Locator{loc}
		p.noteSEDPReceived(peer, entitySEDPPubWriter, 10)
		// Missing/disposed history 1,2,3,7,9 cannot be retransmitted.
		p.handleDiscoveryGap(peer, discoveryGap(order, 1, 3, 7, 0x8a000000))
		hb := discoveryHeartbeat(1, 10)
		p.handleHeartbeat(peer, hb, nil)
		if got := requestedDiscovery(t, receiver); !reflect.DeepEqual(got, []SequenceNumber{4, 5, 6, 8}) {
			t.Fatalf("requested received/gapped history: %v", got)
		}
		for _, sn := range []SequenceNumber{8, 6, 5, 4} {
			p.noteSEDPReceived(peer, entitySEDPPubWriter, sn)
		}
		assertDiscoverySilent(t, p, peer, hb)
	}
}

func TestDiscoveryReplayWindowAndHistoryTrimming(t *testing.T) {
	p := wireParticipant(t)
	receiver, loc := listener(t)
	peer := GUIDPrefix{1, 15, 97}
	p.peers[peer] = []Locator{loc}
	hb := discoveryHeartbeat(300, 600)
	p.noteSEDPReceived(peer, entitySEDPPubWriter, 600)
	p.handleHeartbeat(peer, hb, nil)
	got := requestedDiscovery(t, receiver)
	if len(got) != 256 || got[0] != 300 || got[255] != 555 {
		t.Fatalf("bad bounded replay: %v", got)
	}
	for sn := SequenceNumber(555); sn >= 300; sn-- {
		p.noteSEDPReceived(peer, entitySEDPPubWriter, sn)
	}
	p.handleHeartbeat(peer, hb, nil)
	got = requestedDiscovery(t, receiver)
	if len(got) != 44 || got[0] != 556 || got[43] != 599 {
		t.Fatalf("bad remaining replay: %v", got)
	}
	// The publisher retires the remaining history.
	assertDiscoverySilent(t, p, peer, discoveryHeartbeat(600, 600))
}

func TestDiscoveryRejectsInvalidGap(t *testing.T) {
	for _, s := range []Submessage{
		discoveryGap(binary.LittleEndian, 0, 2, 0),
		discoveryGap(binary.LittleEndian, 3, 2, 0),
		discoveryGap(binary.LittleEndian, 1, 2, 257),
		discoveryGap(binary.LittleEndian, 1, 2, 32),
		discoveryGap(binary.LittleEndian, 1, 1<<63-1, 2, 0xc0000000),
	} {
		if _, err := parseGap(s); err == nil {
			t.Fatal("accepted malformed GAP")
		}
	}
}

func TestDiscoveryGapDestination(t *testing.T) {
	p := wireParticipant(t)
	peer := GUIDPrefix{1, 15, 96}
	gap := discoveryGap(binary.LittleEndian, 1, 11, 0)
	data := buildSubmessage(subGAP, 0, gap.Body)
	p.handle(buildMessage(peer, buildInfoDst(GUIDPrefix{9}), data), nil)
	key := writerKey{prefix: peer, entity: entitySEDPPubWriter}
	if p.sedpReceived[key] != nil {
		t.Fatal("accepted GAP directed at another participant")
	}
	p.handle(buildMessage(peer, buildInfoDst(p.prefix), data), nil)
	if _, missing := p.sedpReceived[key].firstMissing(1, 10); missing {
		t.Fatal("ignored our GAP")
	}
}

func TestDiscoverySequenceBookkeepingBounded(t *testing.T) {
	var s discoverySequences
	for sn := SequenceNumber(2); sn < 2000; sn += 2 {
		s.add(sn, sn)
	}
	if len(s.ranges) > maxDiscoveryRanges {
		t.Fatal("unbounded sparse sequence tracking")
	}
	if sn, missing := s.firstMissing(1, 2000); !missing || sn != 1 {
		t.Fatal("forgot earliest hole")
	}
	s.add(1, 2000)
	if len(s.ranges) != 1 {
		t.Fatal("contiguous recovery did not compact")
	}
	if _, missing := s.firstMissing(1, 2000); missing {
		t.Fatal("complete interval still missing")
	}
	s.add(1<<63-1, 1<<63-1)
	if _, missing := s.firstMissing(1<<63-1, 1<<63-1); missing {
		t.Fatal("maximum sequence overflow")
	}
}
