package rtps

import (
	"encoding/binary"
	"testing"
	"time"
)

func TestSubscriptionSnapshotGapsOnlyRetiredHistory(t *testing.T) {
	p := wireParticipant(t)
	endpoint := func(id uint32) Endpoint {
		return Endpoint{GUID: GUID{Prefix: GUIDPrefix{9}, EntityID: id}, Topic: "rt/camera", Type: "Image"}
	}
	// Keep readers at sequences 1, 4 and 7, with retired instances between.
	for _, id := range []uint32{1, 2, 3, 4, 5} {
		if err := p.Subscribe(endpoint(id)); err != nil {
			t.Fatal(err)
		}
		if id%2 == 0 {
			p.Unsubscribe(endpoint(id).GUID)
		}
	}
	peer, locator := listener(t)
	prefix := GUIDPrefix{9}
	p.mu.Lock()
	p.peers[prefix] = []Locator{locator}
	p.announceSubscriptionsLocked()
	p.mu.Unlock()
	// Snapshot must preserve every active reader and explicitly skip only the
	// removed ones. Replaying an old active reader after a GAP over it is wrong.
	want := []struct {
		kind        byte
		first, next SequenceNumber
	}{
		{subDATA, 1, 0}, {subGAP, 2, 4}, {subDATA, 4, 0}, {subGAP, 5, 7}, {subDATA, 7, 0},
	}
	buf := make([]byte, 4096)
	for _, expected := range want {
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := ParseMessage(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		if len(msg.Submessages) != 2 || msg.Submessages[0].Kind != subINFO_DST || string(msg.Submessages[0].Body) != string(prefix[:]) {
			t.Fatalf("missing directed destination: %+v", msg)
		}
		sub := msg.Submessages[1]
		if sub.Kind != expected.kind {
			t.Fatalf("kind=%x want=%x", sub.Kind, expected.kind)
		}
		if sub.Kind == subDATA {
			data, err := ParseData(sub)
			if err != nil || data.WriterSN != expected.first {
				t.Fatalf("DATA=%+v error=%v", data, err)
			}
		} else {
			b := sub.Body
			if binary.BigEndian.Uint32(b[:4]) != entitySEDPSubReader || binary.BigEndian.Uint32(b[4:8]) != entitySEDPSubWriter || binary.LittleEndian.Uint32(b[12:16]) != uint32(expected.first) || binary.LittleEndian.Uint32(b[20:24]) != uint32(expected.next) || binary.LittleEndian.Uint32(b[24:28]) != 0 {
				t.Fatalf("invalid GAP: %x", b)
			}
		}
	}
}

func TestDirectedDiscoveryReachesParticipantsSharingALocator(t *testing.T) {
	p := wireParticipant(t)
	peer, locator := listener(t)
	p.peers[GUIDPrefix{9}] = []Locator{locator, locator}
	p.peers[GUIDPrefix{10}] = []Locator{locator}
	p.sendDiscoveryLocked(buildMessage(p.prefix, buildGap(entitySEDPSubReader, entitySEDPSubWriter, 1, 2)))
	seen := map[GUIDPrefix]bool{}
	for range 2 {
		buf := make([]byte, 4096)
		_ = peer.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		msg, err := ParseMessage(buf[:n])
		if err != nil {
			t.Fatal(err)
		}
		var prefix GUIDPrefix
		copy(prefix[:], msg.Submessages[0].Body)
		seen[prefix] = true
	}
	if !seen[GUIDPrefix{9}] || !seen[GUIDPrefix{10}] {
		t.Fatalf("destinations=%v", seen)
	}
}

func TestGapEncodesSequenceHighWords(t *testing.T) {
	first, next := SequenceNumber(1<<34+7), SequenceNumber(1<<35+9)
	msg, err := ParseMessage(buildMessage(GUIDPrefix{}, buildGap(entitySEDPSubReader, entitySEDPSubWriter, first, next)))
	if err != nil {
		t.Fatal(err)
	}
	b := msg.Submessages[0].Body
	read := func(b []byte) SequenceNumber {
		return SequenceNumber(int64(binary.LittleEndian.Uint32(b[:4]))<<32 | int64(binary.LittleEndian.Uint32(b[4:8])))
	}
	if read(b[8:16]) != first || read(b[16:24]) != next {
		t.Fatalf("GAP sequence encoding: %x", b)
	}
}
