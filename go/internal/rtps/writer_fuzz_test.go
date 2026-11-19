package rtps

import (
	"net"
	"testing"
)

// FuzzPublishingParticipantDatagrams feeds a publishing participant the
// datagrams only it acts on: subscription announcements, their heartbeats and
// gaps, and requests to resend. Whatever a peer sends, the participant must
// not panic, must keep its tables bounded, and must only ever aim samples at
// a unicast IPv4 address.
func FuzzPublishingParticipantDatagrams(f *testing.F) {
	peer := GUIDPrefix{1, 16, 99}
	self := GUIDPrefix{1, 15, 43}
	reader := GUID{Prefix: peer, EntityID: 0x00000404}
	const writerEntity = 1<<8 | entityUserWriterNoKey
	loc := udpv4Locator(net.IPv4(127, 0, 0, 1), 7411)
	f.Add(subscriptionMessage(reader, testTopic, testType, 1, loc), ackMessage(peer, &self, reader.EntityID, writerEntity, 1, []SequenceNumber{1, 2}, 1))
	f.Add(buildMessage(peer, buildHeartbeat(entitySEDPSubReader, entitySEDPSubWriter, 1, 300, 1, false)), buildMessage(peer, buildGap(entitySEDPSubReader, entitySEDPSubWriter, 2, 9)))
	f.Add(ackMessage(peer, nil, entitySEDPPubReader, entitySEDPPubWriter, 1, []SequenceNumber{1}, 1), buildMessage(peer, buildDispose(entitySEDPSubWriter, 2, reader)))
	f.Add(buildMessage(peer, buildSubmessage(subACKNACK, 0, acknackBody(1, 256, make([]uint32, 8), 1))), buildMessage(peer, buildSubmessage(subGAP, 0, make([]byte, 60))))

	f.Fuzz(func(t *testing.T, first, second []byte) {
		if len(first) > 4096 || len(second) > 4096 {
			return
		}
		p := newParticipant(Config{DomainID: 200, Interface: "lo", Publish: true})
		p.prefix = self
		p.local = net.IPv4(127, 0, 0, 1)
		// A closed socket: every send fails, so nothing leaves the process
		// however the input steers the destinations.
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: p.local})
		if err != nil {
			t.Skip(err)
		}
		_ = conn.Close()
		p.ucast = conn
		defer p.Close()

		w, err := p.NewWriter(Publication{Topic: testTopic, Type: testType})
		if err != nil {
			t.Fatal(err)
		}
		b := newPLBuilder()
		b.addGUID(pidParticipantGUID, GUID{Prefix: peer, EntityID: entityParticipant})
		b.addLocator(pidMetatrafficUnicastLocator, loc)
		b.addLocator(pidDefaultUnicastLocator, loc)
		p.handleSPDP(peer, &DataSubmessage{WriterID: entitySPDPWriter, WriterSN: 1, Payload: b.finish()})

		for _, dgram := range [][]byte{first, second, first} {
			p.handle(append([]byte(nil), dgram...), nil)
			_ = w.Write([]byte{0x00, 0x01, 0x00, 0x00, 1})
		}

		p.mu.Lock()
		defer p.mu.Unlock()
		if len(p.readers) > maxRemoteReaders {
			t.Fatalf("%d remote readers kept", len(p.readers))
		}
		for guid, r := range p.readers {
			if len(r.locators) > maxReaderLocators {
				t.Fatalf("reader %s kept %d locators", guid, len(r.locators))
			}
		}
		if len(w.dests) > len(w.matched) {
			t.Fatalf("%d destinations for %d matched readers", len(w.dests), len(w.matched))
		}
		for _, addr := range w.dests {
			if addr.IP.To4() == nil || addr.IP.IsMulticast() || addr.IP.IsUnspecified() || addr.Port <= 0 || addr.Port > 65535 {
				t.Fatalf("samples aimed at %s", addr)
			}
		}
		// Request state exists only for the publication announcer and for
		// readers this writer is matched to.
		if len(p.acks) > len(w.matched)+len(p.peers) {
			t.Fatalf("%d request states for %d matched readers and %d peers", len(p.acks), len(w.matched), len(p.peers))
		}
		if len(p.subReaders) > len(p.peers) {
			t.Fatalf("receive state for %d writers of %d peers", len(p.subReaders), len(p.peers))
		}
		for _, r := range p.subReaders {
			if r.window.done < 0 {
				t.Fatalf("receive window went backwards: %d", r.window.done)
			}
		}
	})
}
