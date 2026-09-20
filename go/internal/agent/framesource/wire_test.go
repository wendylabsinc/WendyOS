package framesource

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestWire_RoundTripsADescriptorThenFrames(t *testing.T) {
	var buf bytes.Buffer
	src := &agentpbv2.CalibratedSource{Source: "realsense:123", ColourWidth: 640, ColourHeight: 480}
	if err := WriteRecord(&buf, RecordSource, src); err != nil {
		t.Fatalf("writing descriptor: %v", err)
	}
	for i := uint64(1); i <= 3; i++ {
		if err := WriteRecord(&buf, RecordFrame, &agentpbv2.CalibratedFrame{FrameId: i}); err != nil {
			t.Fatalf("writing frame %d: %v", i, err)
		}
	}

	gotSrc, err := ReadSource(&buf)
	if err != nil {
		t.Fatalf("reading descriptor: %v", err)
	}
	if gotSrc.GetSource() != "realsense:123" || gotSrc.GetColourWidth() != 640 {
		t.Errorf("descriptor = %+v", gotSrc)
	}
	for i := uint64(1); i <= 3; i++ {
		f, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("reading frame %d: %v", i, err)
		}
		if f.GetFrameId() != i {
			t.Errorf("frame id = %d, want %d", f.GetFrameId(), i)
		}
	}
	if _, _, err := ReadRecord(&buf); !errors.Is(err, io.EOF) {
		t.Errorf("end of stream = %v, want io.EOF", err)
	}
}

// A helper killed mid-frame must not look like one that finished. The agent
// distinguishes the two to decide whether the camera stopped or the capture
// broke, so this is the difference between a clean end and an error.
func TestWire_TruncatedRecordIsNotACleanEnd(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRecord(&buf, RecordFrame, &agentpbv2.CalibratedFrame{
		FrameId: 1, Colour: make([]byte, 512),
	}); err != nil {
		t.Fatal(err)
	}
	truncated := bytes.NewReader(buf.Bytes()[:buf.Len()-10])
	_, _, err := ReadRecord(truncated)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated record = %v, want io.ErrUnexpectedEOF", err)
	}
	if isCleanEnd(err) {
		t.Error("a truncated record was treated as a clean end of capture")
	}
}

// A length field is attacker-shaped input from another process: honouring it
// blindly would allocate whatever it asked for.
func TestWire_RefusesAnOversizedLengthWithoutAllocatingIt(t *testing.T) {
	header := make([]byte, 5)
	header[0] = RecordFrame
	binary.BigEndian.PutUint32(header[1:], 1<<30)
	_, _, err := ReadRecord(bytes.NewReader(header))
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Errorf("oversized record = %v, want a limit error", err)
	}
}

func TestWire_FrameWhereADescriptorWasExpectedIsAnError(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteRecord(&buf, RecordFrame, &agentpbv2.CalibratedFrame{FrameId: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSource(&buf); err == nil {
		t.Error("a frame was accepted as a source descriptor")
	}
}

// One buffer serves every record on a stream, so the frame handed to the hub
// -- and retained by every subscriber -- must not alias it. If proto.Unmarshal
// ever stopped copying bytes fields, this is the test that would say so.
func TestWire_ReusesItsBufferAndTheFrameDoesNotAliasIt(t *testing.T) {
	var buf bytes.Buffer
	first := &agentpbv2.CalibratedFrame{FrameId: 1, Colour: bytes.Repeat([]byte{1}, 256)}
	second := &agentpbv2.CalibratedFrame{FrameId: 2, Colour: bytes.Repeat([]byte{2}, 256)}
	for _, f := range []*agentpbv2.CalibratedFrame{first, second} {
		if err := WriteRecord(&buf, RecordFrame, f); err != nil {
			t.Fatal(err)
		}
	}
	rr := &recordReader{r: &buf}
	got1, err := readFrame(rr)
	if err != nil {
		t.Fatal(err)
	}
	backing := &rr.buf[:1][0]
	got2, err := readFrame(rr)
	if err != nil {
		t.Fatal(err)
	}
	if &rr.buf[:1][0] != backing {
		t.Error("the second record did not reuse the first record's buffer")
	}
	if !bytes.Equal(got1.GetColour(), first.GetColour()) {
		t.Error("the first frame's colour changed when the buffer was reused: the frame aliases the wire buffer")
	}
	if !bytes.Equal(got2.GetColour(), second.GetColour()) {
		t.Error("the second frame's colour is wrong")
	}
}
