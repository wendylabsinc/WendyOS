package rtps

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"
)

// onlySubmessage parses a datagram holding exactly one submessage.
func onlySubmessage(t *testing.T, sub []byte) Submessage {
	t.Helper()
	msg, err := ParseMessage(buildMessage(GUIDPrefix{1, 15, 7}, sub))
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.Submessages) != 1 {
		t.Fatalf("got %d submessages, want 1", len(msg.Submessages))
	}
	return msg.Submessages[0]
}

func TestBuildHeartbeat_RoundTripsThroughParseHeartbeat(t *testing.T) {
	s := onlySubmessage(t, buildHeartbeat(0x000003c7, 0x000003c2, 1<<33|4, 1<<33|9, 12, true))
	if s.Kind != subHEARTBEAT {
		t.Fatalf("kind = %#x", s.Kind)
	}
	if s.Flags&0x02 == 0 {
		t.Fatal("final flag not set: the reader would have to answer every heartbeat")
	}
	hb, err := ParseHeartbeat(s)
	if err != nil {
		t.Fatal(err)
	}
	want := HeartbeatSubmessage{ReaderID: 0x000003c7, WriterID: 0x000003c2, FirstSN: 1<<33 | 4, LastSN: 1<<33 | 9, Count: 12}
	if *hb != want {
		t.Fatalf("heartbeat = %+v, want %+v", *hb, want)
	}
	if s := onlySubmessage(t, buildHeartbeat(1, 2, 1, 0, 1, false)); s.Flags&0x02 != 0 {
		t.Fatal("final flag set on a heartbeat that asks for an answer")
	}
}

func TestBuildGap_MarksOneRunOfSequenceNumbersIrrelevant(t *testing.T) {
	s := onlySubmessage(t, buildGap(0x00000104, 0x00000203, 5, 9))
	if s.Kind != subGAP {
		t.Fatalf("kind = %#x", s.Kind)
	}
	want := []byte{
		0, 0, 1, 4, // readerId, as octets
		0, 0, 2, 3, // writerId
		0, 0, 0, 0, 5, 0, 0, 0, // gapStart 5 (high, low)
		0, 0, 0, 0, 10, 0, 0, 0, // gapList base: first sequence number after the run
		0, 0, 0, 0, // an empty bitmap: nothing beyond the run
	}
	if !bytes.Equal(s.Body, want) {
		t.Fatalf("body = % x\nwant   % x", s.Body, want)
	}
}

func TestBuildInfoTS_CarriesSecondsAndBinaryFraction(t *testing.T) {
	s := onlySubmessage(t, buildInfoTS(time.Unix(1700000000, 500_000_000)))
	if s.Kind != subINFO_TS || len(s.Body) != 8 {
		t.Fatalf("kind = %#x, body = % x", s.Kind, s.Body)
	}
	if s.Flags&0x02 != 0 {
		t.Fatal("invalidate flag set: the timestamp would be ignored")
	}
	if sec := binary.LittleEndian.Uint32(s.Body[0:4]); sec != 1700000000 {
		t.Fatalf("seconds = %d", sec)
	}
	if frac := binary.LittleEndian.Uint32(s.Body[4:8]); frac != 1<<31 {
		t.Fatalf("fraction = %#x, want half a second (%#x)", frac, uint32(1<<31))
	}
}

func TestParseAcknack_ListsTheRequestedSequenceNumbers(t *testing.T) {
	s := onlySubmessage(t, buildAcknack(0x00000104, 0x00000203, 5, 7, 3))
	ack, err := ParseAcknack(s)
	if err != nil {
		t.Fatal(err)
	}
	if ack.ReaderID != 0x00000104 || ack.WriterID != 0x00000203 || ack.Base != 5 || ack.Count != 3 || ack.Final {
		t.Fatalf("acknack = %+v", ack)
	}
	if len(ack.Missing) != 3 || ack.Missing[0] != 5 || ack.Missing[1] != 6 || ack.Missing[2] != 7 {
		t.Fatalf("missing = %v, want [5 6 7]", ack.Missing)
	}
}

// acknackBody frames an ACKNACK with an explicit bitmap, which buildAcknack
// (always "everything from base") cannot express.
func acknackBody(base uint64, numBits uint32, bitmap []uint32, count uint32) []byte {
	body := make([]byte, 20, 24+4*len(bitmap))
	binary.BigEndian.PutUint32(body[0:4], 0x00000104)
	binary.BigEndian.PutUint32(body[4:8], 0x00000203)
	binary.LittleEndian.PutUint32(body[8:12], uint32(base>>32))
	binary.LittleEndian.PutUint32(body[12:16], uint32(base))
	binary.LittleEndian.PutUint32(body[16:20], numBits)
	for _, word := range bitmap {
		body = binary.LittleEndian.AppendUint32(body, word)
	}
	return binary.LittleEndian.AppendUint32(body, count)
}

func TestParseAcknack_ReadsSparseBitmapAndFinalFlag(t *testing.T) {
	// Bits are numbered from the most significant bit of each word.
	sub := buildSubmessage(subACKNACK, 0x02, acknackBody(40, 34, []uint32{0xa0000000, 0x40000000}, 9))
	ack, err := ParseAcknack(onlySubmessage(t, sub))
	if err != nil {
		t.Fatal(err)
	}
	if !ack.Final || ack.Base != 40 || ack.Count != 9 {
		t.Fatalf("acknack = %+v", ack)
	}
	if len(ack.Missing) != 3 || ack.Missing[0] != 40 || ack.Missing[1] != 42 || ack.Missing[2] != 73 {
		t.Fatalf("missing = %v, want [40 42 73]", ack.Missing)
	}
}

func TestParseAcknack_AcceptsAnEmptySetAsAPureAcknowledgement(t *testing.T) {
	ack, err := ParseAcknack(onlySubmessage(t, buildSubmessage(subACKNACK, 0, acknackBody(12, 0, nil, 1))))
	if err != nil {
		t.Fatal(err)
	}
	if ack.Base != 12 || len(ack.Missing) != 0 {
		t.Fatalf("acknack = %+v", ack)
	}
}

func TestParseAcknack_RejectsMalformedSets(t *testing.T) {
	for name, body := range map[string][]byte{
		"truncated header":        acknackBody(1, 0, nil, 1)[:18],
		"bitmap shorter than set": acknackBody(1, 64, []uint32{0xffffffff}, 1),
		"more than 256 bits":      acknackBody(1, 257, make([]uint32, 9), 1),
	} {
		if _, err := ParseAcknack(onlySubmessage(t, buildSubmessage(subACKNACK, 0, body))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
