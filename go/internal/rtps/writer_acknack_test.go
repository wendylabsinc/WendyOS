package rtps

import (
	"testing"
)

// ackMessage is a remote reader telling one of our writers what it has. With
// dst set the ACKNACK is addressed to that participant.
func ackMessage(from GUIDPrefix, dst *GUIDPrefix, reader, writer uint32, base SequenceNumber, missing []SequenceNumber, count uint32) []byte {
	var subs [][]byte
	if dst != nil {
		subs = append(subs, buildInfoDst(*dst))
	}
	return buildMessage(from, append(subs, buildAcknackSet(reader, writer, base, missing, count))...)
}

// gaps returns the sequence-number runs the GAP submessages in subs write off,
// as [start, end] pairs.
func gaps(t *testing.T, subs []received) [][2]SequenceNumber {
	t.Helper()
	var out [][2]SequenceNumber
	for _, r := range subs {
		if r.sub.Kind != subGAP {
			continue
		}
		gap, err := ParseGap(r.sub)
		if err != nil {
			t.Fatal(err)
		}
		if len(gap.Extra) != 0 {
			t.Fatalf("gap carries a bitmap: %+v", gap)
		}
		out = append(out, [2]SequenceNumber{gap.Start, gap.ListBase - 1})
	}
	return out
}

func lastHeartbeat(t *testing.T, subs []received, writer uint32) *HeartbeatSubmessage {
	t.Helper()
	heartbeats := ofKind(subs, subHEARTBEAT, writer)
	if len(heartbeats) == 0 {
		t.Fatalf("no heartbeat from writer %#x among %d submessages", writer, len(subs))
	}
	last := heartbeats[len(heartbeats)-1]
	if last.sub.Flags&0x02 == 0 {
		t.Fatal("heartbeat is not final: the reader would answer it, and be answered, without end")
	}
	hb, err := ParseHeartbeat(last.sub)
	if err != nil {
		t.Fatal(err)
	}
	if hb.ReaderID != entityUnknown {
		t.Fatalf("heartbeat is addressed to reader %#x; a directed heartbeat must be answered", hb.ReaderID)
	}
	return hb
}

func TestClosedWriterIsWithdrawnFromPeers(t *testing.T) {
	p, w, _, user := matchedWriter(t)
	meta, metaLoc := listener(t)
	addPeer(t, p, GUIDPrefix{1, 15, 12}, meta, metaLoc)
	readSubmessages(t, meta)

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	withdrawals := ofKind(readSubmessages(t, meta), subDATA, entitySEDPPubWriter)
	if len(withdrawals) != 1 {
		t.Fatalf("peer received %d publication messages on close, want 1", len(withdrawals))
	}
	disposed, guid := discoveryDisposal(withdrawals[0].data)
	if !disposed || guid != w.GUID() || withdrawals[0].data.WriterSN != 2 {
		t.Fatalf("close sent disposed=%v for %s as announcement %d, want the writer disposed as announcement 2", disposed, guid, withdrawals[0].data.WriterSN)
	}
	if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err == nil {
		t.Fatal("a closed writer accepted a sample")
	}
	if got := readSubmessages(t, user); len(got) != 0 {
		t.Fatalf("closed writer sent %d submessages to its reader", len(got))
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if again := ofKind(readSubmessages(t, meta), subDATA, entitySEDPPubWriter); len(again) != 0 {
		t.Fatal("second close withdrew the writer again")
	}

	// A participant that turns up afterwards hears nothing of the old writer.
	late, lateLoc := listener(t)
	addPeer(t, p, GUIDPrefix{1, 15, 13}, late, lateLoc)
	subs := readSubmessages(t, late)
	if got := ofKind(subs, subDATA, entitySEDPPubWriter); len(got) != 0 {
		t.Fatalf("late participant was sent %d publication messages for a closed writer", len(got))
	}
	if hb := lastHeartbeat(t, subs, entitySEDPPubWriter); hb.FirstSN != 3 || hb.LastSN != 2 {
		t.Fatalf("late participant's heartbeat covers %d..%d, want nothing to fetch (3..2)", hb.FirstSN, hb.LastSN)
	}
}

func TestPublicationAcknackIsAnsweredWithWhatTheReaderLacks(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	peer := GUIDPrefix{1, 16, 99}
	addPeer(t, p, peer, meta, metaLoc)
	first, _ := p.NewWriter(Publication{Topic: testTopic, Type: testType}) // announcement 1
	second, _ := p.NewWriter(Publication{Topic: "rt/a", Type: testType})   // announcement 2
	third, _ := p.NewWriter(Publication{Topic: "rt/b", Type: testType})    // announcement 3
	fourth, _ := p.NewWriter(Publication{Topic: "rt/c", Type: testType})   // announcement 4
	_ = second.Close()                                                     // announcement 5 withdraws 2
	_ = fourth.Close()                                                     // announcement 6 withdraws 4
	readSubmessages(t, meta)

	// A reader that has nothing yet asks where the writer stands. It is told
	// the range, and that 2 and 4 are not worth asking for.
	p.handle(ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, nil, 1), nil)
	subs := readSubmessages(t, meta)
	if got := ofKind(subs, subDATA, entitySEDPPubWriter); len(got) != 0 {
		t.Fatalf("resent %d announcements nobody asked for", len(got))
	}
	if hb := lastHeartbeat(t, subs, entitySEDPPubWriter); hb.FirstSN != 1 || hb.LastSN != 6 {
		t.Fatalf("heartbeat covers %d..%d, want 1..6", hb.FirstSN, hb.LastSN)
	}
	if got := gaps(t, subs); len(got) != 2 || got[0] != [2]SequenceNumber{2, 2} || got[1] != [2]SequenceNumber{4, 4} {
		t.Fatalf("gaps = %v, want 2 and 4 written off", got)
	}

	// It then asks for all six.
	p.handle(ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, []SequenceNumber{1, 2, 3, 4, 5, 6}, 2), nil)
	subs = readSubmessages(t, meta)
	resent := map[SequenceNumber]GUID{}
	withdrawn := map[SequenceNumber]GUID{}
	for _, r := range ofKind(subs, subDATA, entitySEDPPubWriter) {
		if disposed, guid := discoveryDisposal(r.data); disposed {
			withdrawn[r.data.WriterSN] = guid
		} else {
			resent[r.data.WriterSN] = parsePublication(t, r.data).endpoint
		}
	}
	if len(resent) != 2 || resent[1] != first.GUID() || resent[3] != third.GUID() {
		t.Fatalf("resent announcements %v, want 1 and 3", resent)
	}
	// A reader that missed a withdrawal would otherwise keep a writer that is gone.
	if len(withdrawn) != 2 || withdrawn[5] != second.GUID() || withdrawn[6] != fourth.GUID() {
		t.Fatalf("resent withdrawals %v, want 5 and 6", withdrawn)
	}
	if got := gaps(t, subs); len(got) != 2 || got[0] != [2]SequenceNumber{2, 2} || got[1] != [2]SequenceNumber{4, 4} {
		t.Fatalf("gaps = %v, want 2 and 4 written off", got)
	}
}

func TestAcknackThatIsStaleCompleteOrForSomeoneElseIsNotAnswered(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	peer := GUIDPrefix{1, 16, 99}
	addPeer(t, p, peer, meta, metaLoc)
	if _, err := p.NewWriter(Publication{Topic: testTopic, Type: testType}); err != nil {
		t.Fatal(err)
	}
	readSubmessages(t, meta)
	answered := func(what string, msg []byte, want bool) {
		t.Helper()
		p.handle(msg, nil)
		if got := len(readSubmessages(t, meta)) != 0; got != want {
			t.Fatalf("%s: answered = %v, want %v", what, got, want)
		}
	}
	other := GUIDPrefix{1, 15, 200}
	answered("addressed to another participant", ackMessage(peer, &other, entitySEDPPubReader, entitySEDPPubWriter, 1, []SequenceNumber{1}, 1), false)
	answered("from a participant that never announced itself", ackMessage(GUIDPrefix{1, 15, 201}, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, []SequenceNumber{1}, 1), false)
	answered("first request", ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, []SequenceNumber{1}, 5), true)
	answered("same request again", ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, []SequenceNumber{1}, 5), false)
	answered("older request arriving late", ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, []SequenceNumber{1}, 4), false)
	answered("reader that has everything", ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 2, nil, 6), false)
	answered("unaddressed request", ackMessage(peer, nil, entitySEDPPubReader, entitySEDPPubWriter, 1, []SequenceNumber{1}, 7), true)
}

func TestAcknackFloodIsAnsweredAtABoundedRate(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	peer := GUIDPrefix{1, 16, 99}
	addPeer(t, p, peer, meta, metaLoc)
	if _, err := p.NewWriter(Publication{Topic: testTopic, Type: testType}); err != nil {
		t.Fatal(err)
	}
	readSubmessages(t, meta)
	for count := uint32(1); count <= 500; count++ {
		p.handle(ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, nil, count), nil)
	}
	if got := len(ofKind(readSubmessages(t, meta), subHEARTBEAT, entitySEDPPubWriter)); got == 0 || got > maxAcknackAnswersPerSecond {
		t.Fatalf("500 requests in a burst were answered %d times, want between 1 and %d", got, maxAcknackAnswersPerSecond)
	}
}

func TestOneRequestIsAnsweredWithABoundedNumberOfAnnouncements(t *testing.T) {
	p := publishingParticipant(t)
	meta, metaLoc := listener(t)
	peer := GUIDPrefix{1, 16, 99}
	addPeer(t, p, peer, meta, metaLoc)
	const writers = maxResendsPerAcknack + 8
	var all []SequenceNumber
	for i := range writers {
		if _, err := p.NewWriter(Publication{Topic: testTopic, Type: testType}); err != nil {
			t.Fatal(err)
		}
		all = append(all, SequenceNumber(i+1))
	}
	readSubmessages(t, meta)

	// A request is a few dozen bytes and names up to 256 announcements. The
	// answer goes wherever the participant's announcement said, so it must
	// not be many times larger than the request that provoked it.
	p.handle(ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, all, 1), nil)
	subs := readSubmessages(t, meta)
	first := ofKind(subs, subDATA, entitySEDPPubWriter)
	if len(first) != maxResendsPerAcknack {
		t.Fatalf("one request was answered with %d announcements, want %d", len(first), maxResendsPerAcknack)
	}
	for i, r := range first {
		if r.data.WriterSN != SequenceNumber(i+1) {
			t.Fatalf("answer %d is announcement %d: the oldest ones must go first so the reader's next request shrinks", i, r.data.WriterSN)
		}
	}
	if hb := lastHeartbeat(t, subs, entitySEDPPubWriter); hb.LastSN != writers {
		t.Fatalf("heartbeat ends at %d, want %d so the reader asks for the rest", hb.LastSN, writers)
	}

	// The reader asks again for what it still lacks and gets it.
	p.handle(ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, maxResendsPerAcknack+1, all[maxResendsPerAcknack:], 2), nil)
	if rest := ofKind(readSubmessages(t, meta), subDATA, entitySEDPPubWriter); len(rest) != writers-maxResendsPerAcknack {
		t.Fatalf("second request was answered with %d announcements, want the remaining %d", len(rest), writers-maxResendsPerAcknack)
	}
}

func TestWriterTellsAReaderWhatItCannotResend(t *testing.T) {
	p, w, reader, user := matchedWriter(t)
	for range 3 {
		if err := w.Write([]byte{0x00, 0x01, 0x00, 0x00}); err != nil {
			t.Fatal(err)
		}
	}
	readSubmessages(t, user)

	// The reader lost samples 2 and 3 and asks for them. Nothing is kept, so
	// it is told they are gone and where the stream stands.
	p.handle(ackMessage(reader.Prefix, &p.prefix, reader.EntityID, w.GUID().EntityID, 2, []SequenceNumber{2, 3}, 1), nil)
	subs := readSubmessages(t, user)
	if len(subs) == 0 || subs[0].sub.Kind != subINFO_DST || string(subs[0].sub.Body) != string(reader.Prefix[:]) {
		t.Fatalf("answer is not addressed to the reader's participant: kinds %#x", kinds(subs))
	}
	if got := gaps(t, subs); len(got) != 1 || got[0] != [2]SequenceNumber{2, 3} {
		t.Fatalf("gaps = %v, want 2..3 written off", got)
	}
	if got := ofKind(subs, subDATA, w.GUID().EntityID); len(got) != 0 {
		t.Fatal("resent a sample")
	}
	if hb := lastHeartbeat(t, subs, w.GUID().EntityID); hb.FirstSN != 4 || hb.LastSN != 3 {
		t.Fatalf("heartbeat covers %d..%d, want an empty range starting at 4", hb.FirstSN, hb.LastSN)
	}

	// A reader that has just matched asks without naming anything.
	p.handle(ackMessage(reader.Prefix, &p.prefix, reader.EntityID, w.GUID().EntityID, 1, nil, 2), nil)
	subs = readSubmessages(t, user)
	if hb := lastHeartbeat(t, subs, w.GUID().EntityID); hb.FirstSN != 4 || hb.LastSN != 3 {
		t.Fatalf("heartbeat covers %d..%d, want an empty range starting at 4", hb.FirstSN, hb.LastSN)
	}
	if got := gaps(t, subs); len(got) != 0 {
		t.Fatalf("gaps = %v for a reader that asked for nothing", got)
	}

	// One that is up to date, or that this writer never matched, gets no answer.
	p.handle(ackMessage(reader.Prefix, &p.prefix, reader.EntityID, w.GUID().EntityID, 4, nil, 3), nil)
	p.handle(ackMessage(reader.Prefix, &p.prefix, 0x00009904, w.GUID().EntityID, 1, []SequenceNumber{1}, 1), nil)
	p.handle(ackMessage(reader.Prefix, &p.prefix, reader.EntityID, 0x00009903, 1, []SequenceNumber{1}, 1), nil)
	if got := readSubmessages(t, user); len(got) != 0 {
		t.Fatalf("%d submessages sent in answer to requests that need none", len(got))
	}
}

func TestReadOnlyParticipantIgnoresAcknacks(t *testing.T) {
	p := wireParticipant(t)
	meta, metaLoc := listener(t)
	peer := GUIDPrefix{1, 16, 99}
	addPeer(t, p, peer, meta, metaLoc)
	readSubmessages(t, meta)
	p.handle(ackMessage(peer, &p.prefix, entitySEDPPubReader, entitySEDPPubWriter, 1, nil, 1), nil)
	if got := readSubmessages(t, meta); len(got) != 0 {
		t.Fatalf("read-only participant answered an ACKNACK with %d submessages", len(got))
	}
}
