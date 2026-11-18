package rtps

import (
	"net"
	"testing"
)

// subscriptionMessage is a remote participant announcing one of its readers.
func subscriptionMessage(reader GUID, topic, typ string, sn SequenceNumber, locators ...Locator) []byte {
	b := newPLBuilder()
	b.addGUID(pidParticipantGUID, GUID{Prefix: reader.Prefix, EntityID: entityParticipant})
	b.addGUID(pidEndpointGUID, reader)
	b.addString(pidTopicName, topic)
	b.addString(pidTypeName, typ)
	for _, loc := range locators {
		b.addLocator(pidUnicastLocator, loc)
	}
	return buildMessage(reader.Prefix, buildData(entityUnknown, entitySEDPSubWriter, sn, b.finish()))
}

// matchedWriter returns a writer with one remote reader matched to it. The
// reader takes samples at the returned socket, its participant's default.
func matchedWriter(t *testing.T) (*Participant, *Writer, GUID, *net.UDPConn) {
	t.Helper()
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	user, userLoc := listener(t)
	peer := GUIDPrefix{1, 16, 99}
	addPeer(t, p, peer, meta, metaLoc, userLoc)
	w, err := p.NewWriter(Publication{Topic: testTopic, Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	reader := GUID{Prefix: peer, EntityID: 0x00000404}
	p.handle(subscriptionMessage(reader, testTopic, testType, 1), nil)
	readSubmessages(t, user) // the heartbeat sent on match
	return p, w, reader, user
}

func kinds(subs []received) []uint8 {
	out := make([]uint8, len(subs))
	for i, r := range subs {
		out[i] = r.sub.Kind
	}
	return out
}

func TestWriteSendsEachSampleToTheMatchedReader(t *testing.T) {
	_, w, _, user := matchedWriter(t)
	payload := []byte{0x00, 0x01, 0x00, 0x00, 1, 2, 3, 4, 5}
	for want := SequenceNumber(1); want <= 2; want++ {
		if err := w.Write(payload); err != nil {
			t.Fatal(err)
		}
		subs := readSubmessages(t, user)
		if got := kinds(subs); len(got) != 3 || got[0] != subINFO_TS || got[1] != subDATA || got[2] != subHEARTBEAT {
			t.Fatalf("sample %d arrived as submessages %#x, want timestamp, data, heartbeat", want, got)
		}
		d := subs[1].data
		if d.WriterID != w.GUID().EntityID || d.ReaderID != entityUnknown || d.WriterSN != want {
			t.Fatalf("data from %#x to %#x with sequence number %d", d.WriterID, d.ReaderID, d.WriterSN)
		}
		// Padded to four bytes so the heartbeat that follows is aligned, with
		// the padding length recorded in the encapsulation options.
		if wantPayload := []byte{0x00, 0x01, 0x00, 0x03, 1, 2, 3, 4, 5, 0, 0, 0}; string(d.Payload) != string(wantPayload) {
			t.Fatalf("payload = % x\nwant      % x", d.Payload, wantPayload)
		}
		hb, err := ParseHeartbeat(subs[2].sub)
		if err != nil {
			t.Fatal(err)
		}
		// Nothing is kept for resending: the heartbeat says so, which lets a
		// reliable reader skip a lost sample instead of waiting for it.
		if hb.WriterID != w.GUID().EntityID || hb.FirstSN != want+1 || hb.LastSN != want {
			t.Fatalf("heartbeat = %+v", hb)
		}
		if subs[2].sub.Flags&0x02 == 0 {
			t.Fatal("heartbeat demands an acknowledgement for every sample")
		}
	}
	if string(payload) != string([]byte{0x00, 0x01, 0x00, 0x00, 1, 2, 3, 4, 5}) {
		t.Fatal("Write changed the caller's payload")
	}
}

func TestHeartbeatCountsOnlyGoUp(t *testing.T) {
	_, w, _, user := matchedWriter(t)
	var last uint32
	for range 3 {
		if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err != nil {
			t.Fatal(err)
		}
		heartbeats := ofKind(readSubmessages(t, user), subHEARTBEAT, w.GUID().EntityID)
		if len(heartbeats) != 1 {
			t.Fatalf("got %d heartbeats for one sample", len(heartbeats))
		}
		hb, _ := ParseHeartbeat(heartbeats[0].sub)
		// A reader drops a heartbeat whose count it has already seen.
		if hb.Count <= last {
			t.Fatalf("heartbeat count %d after %d", hb.Count, last)
		}
		last = hb.Count
	}
}

func TestNewlyMatchedReaderIsToldWhereTheStreamStands(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	user, userLoc := listener(t)
	peer := GUIDPrefix{1, 15, 99}
	addPeer(t, p, peer, meta, metaLoc, userLoc)
	w, err := p.NewWriter(Publication{Topic: testTopic, Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		// Nobody is listening yet. The samples are dropped, not queued.
		if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err != nil {
			t.Fatal(err)
		}
	}
	if got := readSubmessages(t, user); len(got) != 0 {
		t.Fatalf("%d submessages sent to a participant with no matching reader", len(got))
	}

	p.handle(subscriptionMessage(GUID{Prefix: peer, EntityID: 0x00000404}, testTopic, testType, 1), nil)
	heartbeats := ofKind(readSubmessages(t, user), subHEARTBEAT, w.GUID().EntityID)
	if len(heartbeats) != 1 {
		t.Fatalf("matched reader received %d heartbeats, want 1", len(heartbeats))
	}
	hb, _ := ParseHeartbeat(heartbeats[0].sub)
	// A reliable reader waits for sample 1 unless it is told 1..3 are gone.
	if hb.FirstSN != 4 || hb.LastSN != 3 {
		t.Fatalf("heartbeat covers %d..%d, want an empty range starting at 4", hb.FirstSN, hb.LastSN)
	}
	if hb.ReaderID != entityUnknown || heartbeats[0].sub.Flags&0x02 == 0 {
		t.Fatal("heartbeat forces an acknowledgement, which would be answered with another heartbeat")
	}
}

func TestReaderAnnouncedBeforeTheWriterIsMatched(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	user, userLoc := listener(t)
	peer := GUIDPrefix{1, 16, 99}
	addPeer(t, p, peer, meta, metaLoc, userLoc)
	p.handle(subscriptionMessage(GUID{Prefix: peer, EntityID: 0x00000404}, testTopic, testType, 1), nil)
	w, err := p.NewWriter(Publication{Topic: testTopic, Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	if w.Matched() != 1 {
		t.Fatalf("matched readers = %d, want 1", w.Matched())
	}
	readSubmessages(t, user)
	if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if got := ofKind(readSubmessages(t, user), subDATA, w.GUID().EntityID); len(got) != 1 {
		t.Fatalf("reader received %d samples, want 1", len(got))
	}
}

func TestSamplesGoToTheReadersOwnLocatorWhenItNamesOne(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	participantDefault, defaultLoc := listener(t)
	own, ownLoc := listener(t)
	peer := GUIDPrefix{1, 15, 99}
	addPeer(t, p, peer, meta, metaLoc, defaultLoc)
	w, err := p.NewWriter(Publication{Topic: testTopic, Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	p.handle(subscriptionMessage(GUID{Prefix: peer, EntityID: 0x00000404}, testTopic, testType, 1, ownLoc), nil)
	readSubmessages(t, own)
	if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if got := ofKind(readSubmessages(t, own), subDATA, w.GUID().EntityID); len(got) != 1 {
		t.Fatalf("reader's own locator received %d samples, want 1", len(got))
	}
	if got := readSubmessages(t, participantDefault); len(got) != 0 {
		t.Fatalf("participant default locator received %d submessages as well", len(got))
	}
}

func TestTwoReadersBehindOneLocatorGetOneCopy(t *testing.T) {
	p, w, reader, user := matchedWriter(t)
	p.handle(subscriptionMessage(GUID{Prefix: reader.Prefix, EntityID: 0x00000504}, testTopic, testType, 2), nil)
	readSubmessages(t, user)
	if w.Matched() != 2 {
		t.Fatalf("matched readers = %d, want 2", w.Matched())
	}
	if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	// The sample is addressed to every reader of the writer, so one datagram
	// per socket serves them all.
	if got := ofKind(readSubmessages(t, user), subDATA, w.GUID().EntityID); len(got) != 1 {
		t.Fatalf("socket shared by two readers received %d copies, want 1", len(got))
	}
}

func TestSamplesAreNotSentToReadersOfOtherTopicsOrTypes(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	user, userLoc := listener(t)
	peer := GUIDPrefix{1, 16, 99}
	addPeer(t, p, peer, meta, metaLoc, userLoc)
	w, err := p.NewWriter(Publication{Topic: testTopic, Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	p.handle(subscriptionMessage(GUID{Prefix: peer, EntityID: 0x00000404}, "rt/other", testType, 1), nil)
	p.handle(subscriptionMessage(GUID{Prefix: peer, EntityID: 0x00000504}, testTopic, "std_msgs::msg::dds_::String_", 2), nil)
	// A participant may not announce readers that belong to another one: the
	// locator it names would receive samples nobody there asked for.
	stray, strayLoc := listener(t)
	forged := subscriptionMessage(GUID{Prefix: GUIDPrefix{1, 15, 5}, EntityID: 0x00000404}, testTopic, testType, 3, strayLoc)
	p.handle(buildMessage(peer, forged[20:]), nil)
	if w.Matched() != 0 {
		t.Fatalf("matched readers = %d, want 0", w.Matched())
	}
	if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if got := readSubmessages(t, user); len(got) != 0 {
		t.Fatalf("%d submessages sent to a participant with no matching reader", len(got))
	}
	if got := readSubmessages(t, stray); len(got) != 0 {
		t.Fatalf("%d submessages sent to a locator named by a forged announcement", len(got))
	}
}

func TestDisposedReaderAndDepartedParticipantStopReceivingSamples(t *testing.T) {
	p, w, reader, user := matchedWriter(t)
	second := GUID{Prefix: reader.Prefix, EntityID: 0x00000504}
	p.handle(subscriptionMessage(second, testTopic, testType, 2), nil)
	readSubmessages(t, user)
	if w.Matched() != 2 {
		t.Fatalf("matched readers = %d, want 2", w.Matched())
	}

	p.handle(buildMessage(reader.Prefix, buildDispose(entitySEDPSubWriter, 3, reader)), nil)
	if w.Matched() != 1 {
		t.Fatalf("matched readers after one was disposed = %d, want 1", w.Matched())
	}
	// A late copy of the old announcement must not bring the reader back.
	p.handle(subscriptionMessage(reader, testTopic, testType, 1), nil)
	if w.Matched() != 1 {
		t.Fatal("stale announcement revived a disposed reader")
	}

	p.mu.Lock()
	p.removePeerLocked(reader.Prefix)
	p.mu.Unlock()
	if w.Matched() != 0 {
		t.Fatalf("matched readers after the participant left = %d, want 0", w.Matched())
	}
	if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err != nil {
		t.Fatal(err)
	}
	if got := readSubmessages(t, user); len(got) != 0 {
		t.Fatalf("%d submessages sent to a departed participant", len(got))
	}
}

func TestWriteRejectsPayloadsItCannotFrame(t *testing.T) {
	_, w, _, user := matchedWriter(t)
	if err := w.Write([]byte{0x00, 0x01}); err == nil {
		t.Error("accepted a payload without an encapsulation header")
	}
	if err := w.Write(make([]byte, maxWriterPayload+1)); err == nil {
		t.Error("accepted a payload that does not fit one datagram")
	}
	if got := readSubmessages(t, user); len(got) != 0 {
		t.Fatalf("%d submessages sent for rejected payloads", len(got))
	}
	// Several kilobytes is well within one datagram on every platform; the
	// upper bound itself is only reachable where the kernel allows it.
	large := make([]byte, 8*1024)
	large[1] = 0x01
	large[len(large)-1] = 0x7f
	if err := w.Write(large); err != nil {
		t.Fatalf("8 KiB payload: %v", err)
	}
	got := ofKind(readSubmessages(t, user), subDATA, w.GUID().EntityID)
	if len(got) != 1 || got[0].data.WriterSN != 1 {
		t.Fatalf("8 KiB payload did not arrive as sample 1 (rejected payloads must not use a sequence number): %d data submessages", len(got))
	}
	if string(got[0].data.Payload) != string(large) {
		t.Fatal("8 KiB payload arrived changed")
	}
}

func TestWriteReportsASampleThatCouldNotBeSent(t *testing.T) {
	p, w, _, _ := matchedWriter(t)
	_ = p.ucast.Close()
	if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err == nil {
		t.Fatal("Write reported success for a sample that never left")
	}
}

func TestReadOnlyParticipantIgnoresSubscriptionAnnouncements(t *testing.T) {
	p := wireParticipant(t)
	peer := GUIDPrefix{1, 16, 99}
	p.handle(subscriptionMessage(GUID{Prefix: peer, EntityID: 0x00000404}, testTopic, testType, 1), nil)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.readers) != 0 {
		t.Fatal("read-only participant keeps remote readers it can never write to")
	}
}
