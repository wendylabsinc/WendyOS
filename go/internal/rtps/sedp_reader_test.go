package rtps

import (
	"net"
	"testing"
	"time"
)

func TestParseGap_ListsTheSequenceNumbersThatWillNeverCome(t *testing.T) {
	gap, err := ParseGap(onlySubmessage(t, buildGap(0x000004c7, 0x000004c2, 5, 9)))
	if err != nil {
		t.Fatal(err)
	}
	if gap.ReaderID != 0x000004c7 || gap.WriterID != 0x000004c2 || gap.Start != 5 || gap.ListBase != 10 || len(gap.Extra) != 0 {
		t.Fatalf("gap = %+v", gap)
	}

	// gapStart 2, then a list starting at 4 whose bitmap also names 4 and 6.
	body := acknackBody(4, 3, []uint32{0xa0000000}, 0)
	body = append(body[:8], append([]byte{0, 0, 0, 0, 2, 0, 0, 0}, body[8:len(body)-4]...)...)
	gap, err = ParseGap(onlySubmessage(t, buildSubmessage(subGAP, 0, body)))
	if err != nil {
		t.Fatal(err)
	}
	if gap.Start != 2 || gap.ListBase != 4 || len(gap.Extra) != 2 || gap.Extra[0] != 4 || gap.Extra[1] != 6 {
		t.Fatalf("gap = %+v, want 2..3 plus 4 and 6", gap)
	}
	if _, err := ParseGap(onlySubmessage(t, buildSubmessage(subGAP, 0, body[:20]))); err == nil {
		t.Fatal("accepted a truncated GAP")
	}
}

func TestReceiveWindowReportsOnlyWhatIsMissing(t *testing.T) {
	var w rxWindow
	if got := w.missing(0); len(got) != 0 {
		t.Fatalf("missing before anything was announced = %v", got)
	}
	w.mark(3)
	if got := w.missing(4); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("missing = %v, want [1 2 4]", got)
	}
	w.mark(1)
	w.mark(2)
	if w.next() != 4 {
		t.Fatalf("next = %d, want 4 once 1..3 have arrived", w.next())
	}
	// The writer no longer has anything below 9.
	w.advanceTo(9)
	w.mark(4) // late copy of something already written off
	if got := w.missing(10); len(got) != 2 || got[0] != 9 || got[1] != 10 {
		t.Fatalf("missing = %v, want [9 10]", got)
	}
	// Far beyond the window: remembered as missing only once the window gets there.
	w.mark(5000)
	if got := w.missing(5000); len(got) != rxWindowBits || got[0] != 9 || got[len(got)-1] != 9+rxWindowBits-1 {
		t.Fatalf("missing spans %d..%d (%d entries), want one full window from 9", got[0], got[len(got)-1], len(got))
	}
}

// sedpExchange is a publishing participant and a peer whose discovery socket
// the test reads, so it sees exactly what the participant answers.
type sedpExchange struct {
	p    *Participant
	peer GUIDPrefix
	conn *net.UDPConn
	src  *net.UDPAddr
}

func newSEDPExchange(t *testing.T, p *Participant) *sedpExchange {
	t.Helper()
	conn, loc := listener(t)
	x := &sedpExchange{p: p, peer: GUIDPrefix{1, 16, 99}, conn: conn, src: conn.LocalAddr().(*net.UDPAddr)}
	addPeer(t, p, x.peer, conn, loc)
	readSubmessages(t, conn) // our SPDP reply
	return x
}

func (x *sedpExchange) heartbeat(first, last SequenceNumber, count uint32, final bool) {
	x.p.handle(buildMessage(x.peer, buildHeartbeat(entitySEDPSubReader, entitySEDPSubWriter, first, last, count, final)), x.src)
}

func (x *sedpExchange) announce(entity uint32, sn SequenceNumber) {
	x.p.handle(subscriptionMessage(GUID{Prefix: x.peer, EntityID: entity}, testTopic, testType, sn), x.src)
}

// acknacks returns the ACKNACKs the peer received, checking that each names
// the peer as its destination and the subscription endpoints.
func (x *sedpExchange) acknacks(t *testing.T) []*AcknackSubmessage {
	t.Helper()
	var out []*AcknackSubmessage
	var dst []byte
	for _, r := range readSubmessages(t, x.conn) {
		switch r.sub.Kind {
		case subINFO_DST:
			dst = r.sub.Body
		case subACKNACK:
			ack, err := ParseAcknack(r.sub)
			if err != nil {
				t.Fatal(err)
			}
			if string(dst) != string(x.peer[:]) {
				t.Fatalf("ACKNACK is not addressed to the peer (INFO_DST % x)", dst)
			}
			if ack.ReaderID != entitySEDPSubReader || ack.WriterID != entitySEDPSubWriter {
				t.Fatalf("ACKNACK from reader %#x to writer %#x", ack.ReaderID, ack.WriterID)
			}
			out = append(out, ack)
		}
	}
	return out
}

func TestSubscriptionHeartbeatIsAnsweredWithWhatIsMissing(t *testing.T) {
	x := newSEDPExchange(t, publishingParticipant(t))
	// Reader 3 was announced live. Readers 1, 2 and 4 existed before this
	// participant joined: their participant only says so in a heartbeat.
	x.announce(0x00000304, 3)
	x.heartbeat(1, 4, 1, true)
	acks := x.acknacks(t)
	if len(acks) != 1 {
		t.Fatalf("got %d ACKNACKs, want 1", len(acks))
	}
	if got := acks[0].Missing; len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("asked for %v, want [1 2 4]", got)
	}
}

func TestSubscriptionRequestGoesToThePeersDiscoveryLocator(t *testing.T) {
	x := newSEDPExchange(t, publishingParticipant(t))
	// Fast DDS sends heartbeats from a socket it never reads. An answer sent
	// back to that socket is lost; the peer listens where its announcement
	// said it does.
	sender, _ := listener(t)
	x.src = sender.LocalAddr().(*net.UDPAddr)
	x.heartbeat(1, 2, 1, true)
	if acks := x.acknacks(t); len(acks) != 1 || len(acks[0].Missing) != 2 {
		t.Fatalf("discovery locator received %+v, want one request for [1 2]", acks)
	}
	if got := readSubmessages(t, sender); len(got) != 0 {
		t.Fatalf("%d submessages were sent to the socket the heartbeat came from", len(got))
	}
}

func TestCompleteSubscriptionHistoryIsAcknowledgedOnlyWhenAsked(t *testing.T) {
	x := newSEDPExchange(t, publishingParticipant(t))
	x.announce(0x00000104, 1)
	x.announce(0x00000204, 2)
	x.heartbeat(1, 2, 1, true)
	if acks := x.acknacks(t); len(acks) != 0 {
		t.Fatalf("answered a final heartbeat with nothing missing: %+v", acks[0])
	}
	// Without the final flag the writer wants an answer; it stops sending
	// heartbeats once it has one.
	x.p.forgetAckTimes()
	x.heartbeat(1, 2, 2, false)
	acks := x.acknacks(t)
	if len(acks) != 1 || len(acks[0].Missing) != 0 || acks[0].Base != 3 || !acks[0].Final {
		t.Fatalf("acknowledgement = %+v, want one final ACKNACK with base 3 and nothing missing", acks)
	}
}

func TestGapsAndAdvancedHeartbeatsAreNotAskedForAgain(t *testing.T) {
	x := newSEDPExchange(t, publishingParticipant(t))
	x.announce(0x00000304, 3)
	// Readers 1 and 2 were closed; the peer says so instead of resending them.
	x.p.handle(buildMessage(x.peer, buildGap(entitySEDPSubReader, entitySEDPSubWriter, 1, 2)), x.src)
	x.heartbeat(1, 4, 1, true)
	acks := x.acknacks(t)
	if len(acks) != 1 || len(acks[0].Missing) != 1 || acks[0].Missing[0] != 4 {
		t.Fatalf("after a gap for 1..2 asked for %+v, want only 4", acks)
	}
	// A heartbeat whose range starts later writes off everything before it.
	x.p.forgetAckTimes()
	x.heartbeat(6, 6, 2, true)
	acks = x.acknacks(t)
	if len(acks) != 1 || len(acks[0].Missing) != 1 || acks[0].Missing[0] != 6 {
		t.Fatalf("after a heartbeat for 6..6 asked for %+v, want only 6", acks)
	}
}

func TestRepeatedHeartbeatsCannotProvokeAnAcknackEach(t *testing.T) {
	x := newSEDPExchange(t, publishingParticipant(t))
	for count := uint32(1); count <= 50; count++ {
		x.heartbeat(1, 4, count, false)
	}
	if acks := x.acknacks(t); len(acks) != 1 {
		t.Fatalf("50 heartbeats in a burst were answered %d times, want once", len(acks))
	}
	x.p.forgetAckTimes()
	x.heartbeat(1, 4, 51, false)
	if acks := x.acknacks(t); len(acks) != 1 {
		t.Fatalf("heartbeat after the pause was answered %d times, want once", len(acks))
	}
}

func TestReadOnlyParticipantStaysSilentOnSubscriptionHeartbeats(t *testing.T) {
	x := newSEDPExchange(t, wireParticipant(t))
	x.heartbeat(1, 4, 1, false)
	if acks := x.acknacks(t); len(acks) != 0 {
		t.Fatal("read-only participant asked a peer for its subscriptions")
	}
}

func TestDepartedPeerSubscriptionStateIsForgotten(t *testing.T) {
	x := newSEDPExchange(t, publishingParticipant(t))
	x.announce(0x00000104, 1)
	x.p.mu.Lock()
	x.p.removePeerLocked(x.peer)
	n := len(x.p.subReaders)
	x.p.mu.Unlock()
	if n != 0 {
		t.Fatal("receive state kept for a departed participant")
	}
	// Until it announces itself again, its heartbeats are a stranger's.
	x.heartbeat(1, 1, 1, false)
	if acks := x.acknacks(t); len(acks) != 0 {
		t.Fatal("answered a heartbeat from a participant that is not known")
	}
	// It comes back with a new history that starts again at 1.
	addPeer(t, x.p, x.peer, x.conn, udpv4Locator(x.src.IP, x.src.Port))
	x.heartbeat(1, 1, 2, true)
	acks := x.acknacks(t)
	if len(acks) != 1 || len(acks[0].Missing) != 1 || acks[0].Missing[0] != 1 {
		t.Fatalf("returning participant was asked for %+v, want [1]", acks)
	}
}

// forgetAckTimes lets the next heartbeat be answered without waiting out the
// minimum interval between ACKNACKs.
func (p *Participant) forgetAckTimes() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range p.subReaders {
		r.lastAck = time.Time{}
	}
}
